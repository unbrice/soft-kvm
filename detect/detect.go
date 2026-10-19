// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// detect.go: HID device enumeration, --trigger suggestions and the
// --declare-hid and --declare-screen declarations that complete the connect
// line (SPEC §5.5, §5.6, §6.1).

package detect

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/telesma-app/hid"
	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/platform"
	"github.com/unbrice/soft-kvm/screen"
)

const logitechVID = 0x046d

// Dim renders a #-comment block for w: dimmed when w is a terminal that
// wants escape codes, unchanged otherwise. detect and the service
// installer both print shell transcripts — prose as #-comments, commands
// at column 0 — and this is how both grey the prose.
func Dim(w io.Writer, s string) string {
	return dimComments(s, platform.WantsColor(w))
}

// dimComments dims the #-comment lines, leaving the commands as the only
// unstyled lines. Each line carries its own reset, so a line-oriented pipe
// never leaks the attribute. color is a parameter, not a wantsColor call,
// so the tests exercise both renderings.
func dimComments(s string, color bool) string {
	if !color {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#") {
			lines[i] = "\x1b[2m" + l + "\x1b[0m"
		}
	}
	return strings.Join(lines, "\n")
}

type deviceKey struct {
	vid, pid uint16
	serial   string
}

type ifaceUsage struct {
	usagePage uint16
	usage     uint16
}

type hidDevice struct {
	key        deviceKey
	mfr        string
	product    string
	interfaces []ifaceUsage
	shadow     bool // kernel-created virtual node of a receiver-paired peripheral
}

// probeBudget bounds the whole HID++ scan, all devices together: one shared
// context, so a slow receiver cannot stretch detect linearly.
const probeBudget = 15 * time.Second

// screenBudget bounds the monitor probe: `ddcutil detect` walks every I²C bus
// and takes seconds on a good day.
const screenBudget = 20 * time.Second

// Run enumerates HID devices and probes the monitor, then prints how to wire
// connect: the suggested invocations first, as a shell transcript, then what
// the declarations need filled in, then the device list they were drawn from.
func Run(ctx context.Context, w io.Writer) error {
	devices, err := enumerateHIDDevices(ctx)
	if err != nil {
		return fmt.Errorf("enumerate HID devices: %w", err)
	}
	probed := probeAll(ctx, devices)
	sctx, cancel := context.WithTimeout(ctx, screenBudget)
	report := screen.Probe(sctx, platform.Output)
	cancel()
	wiring := buildWiring(probed, report, readChannels(ctx, probed))
	if err := renderSuggestions(w, wiring); err != nil {
		return err
	}
	if err := renderNotes(w, report, wiring); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return renderDevices(w, probed, wiring)
}

// warnSign opens a comment the user has to act on.
const warnSign = "⚠️ "

const (
	// inputPlaceholder stands in a screen declaration for the one thing detect
	// cannot always know: which monitor port this host is plugged into.
	inputPlaceholder = "DP1_HDMI_ETC"
	// namePlaceholder stands in when no monitor was found to name.
	namePlaceholder = "NAME"
	// channelPlaceholder stands in a device declaration for the Easy-Switch
	// channel of a device that is not linked to this host.
	channelPlaceholder = "host=N"
)

// screenDecl is one --declare-screen candidate (SPEC §5.6).
type screenDecl struct {
	name  string // the monitor's EDID name
	input string // this host's port alias, or inputPlaceholder
	table string // "" for the standard table
}

// words are the declaration's words, as connect splits the flag's value. The
// name is quoted: connect's splitter reads double quotes the way strconv
// writes them for a printable name, with only \" and \\ escaped (SPEC §5.4).
func (d screenDecl) words() []string {
	w := []string{strconv.Quote(d.name), d.input}
	if d.table != "" {
		w = append(w, d.table)
	}
	return w
}

// complete reports whether the declaration holds no placeholder.
func (d screenDecl) complete() bool { return d.input != inputPlaceholder }

// hidDecl is one --declare-hid candidate (SPEC §5.5).
type hidDecl struct {
	name    string
	target  hidpp.Target
	channel uint8 // 0 when detect could not read it
	// easySwitch is set for a keyboard or mouse the HID++ scan found: the
	// devices a channel means something for.
	easySwitch bool
	// watched is set for a trigger candidate with no channel: it is declared
	// without host=N, which a trigger needs no more than a name.
	watched bool
	// twin is set when another device of the same model is declared and one of
	// them can be sent to a host: their names are numbered by address, an order
	// another host need not share.
	twin bool
}

// complete reports whether the declaration holds no placeholder.
func (d hidDecl) complete() bool { return d.channel != 0 || d.watched }

func (d hidDecl) words() []string {
	w := []string{d.name, d.target.String()}
	switch {
	case d.channel != 0:
		w = append(w, fmt.Sprintf("host=%d", d.channel))
	case !d.watched:
		w = append(w, channelPlaceholder)
	}
	return w
}

// hostWiring is what detect declares about this host. A declaration that
// still holds a placeholder stays out of the suggested command — connect
// would refuse it — and is printed after it, as a comment to complete.
type hostWiring struct {
	screens []screenDecl
	hids    []hidDecl
	// kbdTriggers and mouseTriggers name the declarations that can carry the
	// gesture, sorted.
	kbdTriggers, mouseTriggers []string
}

// takesScreen and takesHID report whether the suggested command declares a
// monitor or a device this host can take.
func (w hostWiring) takesScreen() bool { return slices.ContainsFunc(w.screens, screenDecl.complete) }
func (w hostWiring) takesHID() bool {
	return slices.ContainsFunc(w.hids, func(d hidDecl) bool { return d.channel != 0 })
}

// easySwitch reports whether any declared device can be sent to a host.
func (w hostWiring) easySwitch() bool {
	return slices.ContainsFunc(w.hids, func(d hidDecl) bool { return d.easySwitch })
}

// twins lists the names of the devices that share a model with another, in
// address order: the ones to check against the other hosts.
func (w hostWiring) twins() []string {
	var out []string
	for _, d := range w.hids {
		if d.twin {
			out = append(out, d.name)
		}
	}
	return out
}

// isTwin reports whether the device at t shares its model with another.
func (w hostWiring) isTwin(t hidpp.Target) bool {
	return slices.ContainsFunc(w.hids, func(d hidDecl) bool { return d.twin && d.target == t })
}

// pendingScreens and pendingHIDs are the declarations left to complete.
func (w hostWiring) pendingScreens() []screenDecl {
	return slices.DeleteFunc(slices.Clone(w.screens), screenDecl.complete)
}

func (w hostWiring) pendingHIDs() []hidDecl {
	return slices.DeleteFunc(slices.Clone(w.hids), hidDecl.complete)
}

// declFlag renders --name 'words', the value in single quotes so the user's shell
// hands connect the string whole, any ' inside closed and escaped.
func declFlag(name string, words []string) string {
	return "--" + name + " '" + strings.ReplaceAll(strings.Join(words, " "), "'", `'\''`) + "'"
}

// buildWiring turns what detect saw into the declarations of this host: one
// screen per monitor, one device per keyboard and mouse, and one per trigger
// candidate, which --trigger can only name. channels holds the Easy-Switch
// channel of every device linked here, as readChannels found it.
func buildWiring(devices []probedDevice, r screen.Report, channels map[hidpp.Target]uint8) hostWiring {
	var w hostWiring
	for _, d := range declaredMonitors(r) {
		decl := screenDecl{name: d.Model, input: inputPlaceholder}
		t := screen.TableFor(d.Mfg)
		if t.Name != screen.StandardName {
			decl.table = t.Name
		}
		if d.InputOK {
			if alias, ok := t.AliasFor(d.Input); ok {
				decl.input = alias
			}
		}
		w.screens = append(w.screens, decl)
	}
	if len(w.screens) == 0 {
		w.screens = []screenDecl{{name: namePlaceholder, input: inputPlaceholder}}
	}

	targets := hidTargets(devices)
	easySwitch := len(targets)
	kbd, mouse := triggerTargets(devices)
	has := func(list []kindTarget, t hidpp.Target) bool {
		return slices.ContainsFunc(list, func(k kindTarget) bool { return k.target == t })
	}
	for _, t := range slices.Concat(kbd, mouse) {
		if !has(targets, t.target) {
			targets = append(targets, t)
		}
	}
	decls := make([]hidDecl, len(targets))
	for i, t := range targets {
		decls[i] = hidDecl{target: t.target, channel: channels[t.target], easySwitch: i < easySwitch}
		decls[i].watched = decls[i].channel == 0 && (has(kbd, t.target) || has(mouse, t.target))
		decls[i].name = declName(t.model, t.kind)
	}
	// Two devices of one model are numbered by address, lowest first, and
	// flagged: another host need not order them the same.
	slices.SortStableFunc(decls, func(a, b hidDecl) int { return a.target.Compare(b.target) })
	used := map[string]bool{}
	models := map[string][]int{}
	for i := range decls {
		base := decls[i].name
		models[base] = append(models[base], i)
		for n := 2; used[decls[i].name]; n++ {
			decls[i].name = base + "-" + strconv.Itoa(n)
		}
		used[decls[i].name] = true
	}
	for _, group := range models {
		if len(group) > 1 && slices.ContainsFunc(group, func(i int) bool { return decls[i].easySwitch }) {
			for _, i := range group {
				decls[i].twin = true
			}
		}
	}
	w.hids = decls
	for _, d := range decls {
		if !d.complete() {
			continue
		}
		if has(kbd, d.target) {
			w.kbdTriggers = append(w.kbdTriggers, d.name)
		}
		if has(mouse, d.target) {
			w.mouseTriggers = append(w.mouseTriggers, d.name)
		}
	}
	slices.Sort(w.kbdTriggers)
	slices.Sort(w.mouseTriggers)
	return w
}

// triggerTargets lists the devices that can carry "the user switched input to
// this host", keyboards apart from mice: a receiver's by pairing slot, linked
// right now, and anything else by its own VID:PID, whose HID node really does
// come and go.
func triggerTargets(devices []probedDevice) (kbd, mouse []kindTarget) {
	for _, dev := range devices {
		if !dev.isTrigger() {
			continue
		}
		if dev.isReceiver() {
			for _, p := range dev.triggerSlots() {
				t := kindTarget{p.Kind, p.Name, hidpp.Target{VID: dev.key.vid, PID: dev.key.pid, Slot: p.Index}}
				switch p.Kind {
				case hidpp.KindKeyboard:
					kbd = append(kbd, t)
				case hidpp.KindMouse:
					mouse = append(mouse, t)
				}
			}
			continue
		}
		target := hidpp.Target{VID: dev.key.vid, PID: dev.key.pid}
		if dev.hasKeyboard() {
			kbd = append(kbd, kindTarget{hidpp.KindKeyboard, dev.product, target})
		}
		if dev.status == probeOK && dev.inv.Kind == hidpp.KindMouse {
			mouse = append(mouse, kindTarget{hidpp.KindMouse, dev.product, target})
		}
	}
	return kbd, mouse
}

// declName names a device after its model, as detect does on every host, so
// that the hosts agree on it: "MX Master 3S" is mx-master-3s. A device whose
// model is unknown is named after its kind.
func declName(model string, kind hidpp.Kind) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(model) {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			dash = b.Len() > 0
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		b.WriteRune(r)
	}
	// The receiver stores the model alone; a Bluetooth name may lead with the
	// brand.
	name := strings.TrimPrefix(b.String(), "logitech-")
	if !hidpp.ValidName(name) {
		return kind.String()
	}
	return name
}

// kindTarget is a keyboard or mouse, its model, and where this host reaches
// it.
type kindTarget struct {
	kind   hidpp.Kind
	model  string
	target hidpp.Target
}

// hidTargets lists the keyboards and mice the HID++ scan found: a receiver's
// are declared per pairing slot — its bare VID:PID is always present, so
// take-hid would post it as arrived and nothing would move — and a device
// attached on its own by its VID:PID (SPEC §5.5). Kernel shadows are left
// out: their receiver names them.
func hidTargets(devices []probedDevice) []kindTarget {
	var out []kindTarget
	for _, dev := range devices {
		if dev.status != probeOK || dev.inv == nil || dev.shadow {
			continue
		}
		switch dev.inv.Kind {
		case hidpp.KindReceiver:
			for _, p := range dev.inv.Paired {
				if p.Kind == hidpp.KindKeyboard || p.Kind == hidpp.KindMouse {
					out = append(out, kindTarget{p.Kind, p.Name, hidpp.Target{VID: dev.key.vid, PID: dev.key.pid, Slot: p.Index}})
				}
			}
		case hidpp.KindKeyboard, hidpp.KindMouse:
			out = append(out, kindTarget{dev.inv.Kind, dev.product, hidpp.Target{VID: dev.key.vid, PID: dev.key.pid}})
		}
	}
	return out
}

// channelBudget bounds reading one device's channel: an RF round trip at worst.
const channelBudget = 3 * time.Second

// readChannels reads the Easy-Switch channel this host occupies on each
// device linked to it. A device that is not linked is left out: no other host
// can derive its channel, and detect cannot ask it (SPEC §5.5).
func readChannels(ctx context.Context, devices []probedDevice) map[hidpp.Target]uint8 {
	linked := map[hidpp.Target]bool{}
	for _, dev := range devices {
		if dev.status != probeOK || dev.inv == nil || dev.shadow {
			continue
		}
		if dev.inv.Kind == hidpp.KindReceiver {
			for _, p := range dev.inv.Paired {
				linked[hidpp.Target{VID: dev.key.vid, PID: dev.key.pid, Slot: p.Index}] = p.Online
			}
			continue
		}
		linked[hidpp.Target{VID: dev.key.vid, PID: dev.key.pid}] = true
	}
	channels := map[hidpp.Target]uint8{}
	for _, kt := range hidTargets(devices) {
		if !linked[kt.target] {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, channelBudget)
		ch, err := hidpp.CurrentChannel(cctx, kt.target.VID, kt.target.PID, kt.target.Slot)
		cancel()
		if err == nil {
			channels[kt.target] = ch
		}
	}
	return channels
}

// declaredMonitors picks the monitors the declarations are written for: every
// one that answers DDC, else — when none does — one known by its EDID alone,
// whose brand still names the table. Among those a vendor-table monitor wins:
// a laptop panel never answers DDC and maps to no table.
func declaredMonitors(r screen.Report) []screen.Display {
	if len(r.Displays) > 0 {
		return r.Displays
	}
	for _, d := range r.Unreachable {
		if screen.TableFor(d.Mfg).Name != screen.StandardName {
			return []screen.Display{d}
		}
	}
	if len(r.Unreachable) > 0 {
		return r.Unreachable[:1]
	}
	return nil
}

// renderNotes explains what the suggestions leave open: the monitors and
// tables that were found, how to fill the placeholders in and, last, why the
// backend cannot reach a monitor when it cannot.
func renderNotes(w io.Writer, r screen.Report, wiring hostWiring) error {
	var b strings.Builder
	b.WriteString("\n")
	note := func(text string) {
		for _, line := range wrapAt(text, 72) {
			fmt.Fprintf(&b, "# %s\n", line)
		}
	}
	for _, d := range declaredMonitors(r) {
		t := screen.TableFor(d.Mfg)
		line := fmt.Sprintf("# Monitor: %s (%s), mapping table %s", d.Model, d.Mfg, t.Name)
		if a, ok := t.AliasFor(d.Input); ok && d.InputOK {
			line += ", showing this host on " + a
		}
		b.WriteString(line + ".\n")
	}
	// pending lists declarations the command left out, with the take they
	// need when the command has none of that kind.
	pending := func(title string, flags []string, take string) {
		b.WriteString("\n")
		note(warnSign + title)
		for _, f := range flags {
			fmt.Fprintf(&b, "#   %s\n", f)
		}
		if take != "" {
			fmt.Fprintf(&b, "#   -A %s\n", take)
		}
	}
	if screens := wiring.pendingScreens(); len(screens) > 0 {
		var flags []string
		for _, d := range screens {
			flags = append(flags, declFlag("declare-screen", d.words()))
		}
		take := ""
		if !wiring.takesScreen() {
			take = "take-screen"
		}
		pending("Add these flags for the monitor to move as well:", flags, take)
		table := screen.TableFor("")
		if ms := declaredMonitors(r); len(ms) > 0 {
			table = screen.TableFor(ms[0].Mfg)
		}
		note(fmt.Sprintf("Replace %s with this computer's port on the monitor (%s, or a raw value such as 0x1B), or switch the monitor to this host and run detect again.",
			inputPlaceholder, strings.Join(table.Aliases(), ", ")))
	}
	if hids := wiring.pendingHIDs(); len(hids) > 0 {
		var flags []string
		for _, d := range hids {
			flags = append(flags, declFlag("declare-hid", d.words()))
		}
		take := ""
		if !wiring.takesHID() {
			take = "take-hid"
		}
		pending("Add flags for the following devices if you want them to move as well:", flags, take)
		note(fmt.Sprintf("Replace %s with this computer's number on the device, or switch the device to this host and run detect again.", channelPlaceholder))
	}
	for _, d := range wiring.hids {
		if d.watched && d.easySwitch {
			b.WriteString("\n")
			note(fmt.Sprintf("%sdetect could not read the Easy-Switch channel of %s: add host=N to its declaration (1-%d, as printed on the key) for this host to take it back.",
				warnSign, d.name, hidpp.MaxChannel))
		}
	}
	switch names := wiring.twins(); {
	case len(names) > 0:
		b.WriteString("\n")
		note(twinMark + "Device names must be the same on every host.")
		note(fmt.Sprintf("Check that %s name the same devices on every host: detect numbers devices of one model by address, and another host may order them differently.",
			joinNames(names)))
		for i, line := range wrapAt(warnSign+"A name that means a different device there makes take-hid move the wrong one.", 72) {
			prefix := "# "
			if i > 0 {
				prefix = "#    "
			}
			fmt.Fprintf(&b, "%s%s\n", prefix, line)
		}
	case wiring.easySwitch():
		b.WriteString("\n")
		note("Device names must be the same on every host.")
	}
	if _, err := io.WriteString(w, Dim(w, b.String())); err != nil {
		return err
	}
	if r.Err == nil {
		return nil
	}
	b.Reset()
	prefix := "# " + warnSign
	for line := range strings.Lines(r.Err.Error()) {
		fmt.Fprintf(&b, "%s%s\n", prefix, strings.TrimRight(line, "\n"))
		prefix = "#    "
	}
	_, err := io.WriteString(w, Dim(w, b.String()))
	return err
}

// joinNames lists names the way prose does: "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func enumerateHIDDevices(ctx context.Context) ([]*hidDevice, error) {
	groups := make(map[deviceKey]*hidDevice)
	for info, err := range hid.Enumerate() {
		if err != nil {
			return nil, err
		}
		// hid.Enumerate takes no context, but it is an iterator: check per
		// iteration and return early — the range-over-func defers run.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if info.VendorID == 0 {
			continue
		}
		key := deviceKey{vid: info.VendorID, pid: info.ProductID, serial: info.SerialNbr}
		dev, ok := groups[key]
		if !ok {
			dev = &hidDevice{
				key:     key,
				mfr:     info.MfrStr,
				product: info.ProductStr,
			}
			groups[key] = dev
		}
		dev.interfaces = append(dev.interfaces, ifaceUsage{usagePage: info.UsagePage, usage: info.Usage})
		dev.shadow = dev.shadow || kernelShadow(info.Path)
	}
	devices := make([]*hidDevice, 0, len(groups))
	for _, dev := range groups {
		devices = append(devices, dev)
	}
	slices.SortFunc(devices, func(a, b *hidDevice) int {
		if c := cmp.Compare(a.key.vid, b.key.vid); c != 0 {
			return c
		}
		if c := cmp.Compare(a.key.pid, b.key.pid); c != 0 {
			return c
		}
		return strings.Compare(a.key.serial, b.key.serial)
	})
	return devices, nil
}

// probeStatus is how one device's HID++ scan ended; the zero value is
// probeSkipped, for devices that are not Logitech's and were never
// attempted.
type probeStatus int

const (
	probeSkipped  probeStatus = iota
	probeOK                   // inv is set
	probeFailed               // err says why: permission, no answer, ...
	probeNotHIDPP             // every interface rejected the report: cannot speak HID++
)

// The named scan failures, so the display can pick an icon per cause.
var (
	errNoAnswer   = errors.New("no answer")
	errPermission = errors.New("permission denied")
	errNotHIDPP   = errors.New("no interface carries the HID++ report")
)

// probedDevice is a hidDevice plus the outcome of its HID++ scan. The
// enumeration result is embedded by value: probing produces new structs,
// it never mutates the enumerated ones.
type probedDevice struct {
	hidDevice
	status probeStatus
	inv    *hidpp.Inventory
	err    error
}

// scanFailure renders a failed scan for display.
func (d probedDevice) scanFailure() string {
	switch {
	case errors.Is(d.err, errPermission):
		return "🔒 HID++ scan denied — " + permissionRemediation
	case errors.Is(d.err, errNoAnswer):
		return "⏳ no answer — the device did not speak HID++"
	default:
		return "⚠️ " + d.err.Error()
	}
}

// classifyProbeErr maps the raw probe error onto the named failures.
func classifyProbeErr(err error) error {
	switch {
	case errors.Is(err, os.ErrPermission):
		return errPermission
	case errors.Is(err, context.DeadlineExceeded):
		return errNoAnswer
	case errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.EINVAL):
		// Every interface rejected the report on write: its report
		// descriptor has no HID++ report, so the device cannot speak HID++.
		return errNotHIDPP
	default:
		return err
	}
}

// probeAll scans Logitech devices in parallel and returns every device,
// scanned or not, as a probedDevice. One context bounds the whole batch; each
// probe joins the context's cause into its error, so a scan killed by the
// budget is reported as failed rather than silently skipped, and the drain
// loop never waits on anything but the results channel. The channel is
// buffered so no sender can block.
func probeAll(ctx context.Context, devices []*hidDevice) []probedDevice {
	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	type result struct {
		i   int
		inv *hidpp.Inventory
		err error
	}
	out := make([]probedDevice, len(devices))
	ch := make(chan result, len(devices))
	n := 0
	for i, dev := range devices {
		out[i].hidDevice = *dev
		if !dev.shouldProbe() {
			continue
		}
		n++
		go func() {
			inv, err := hidpp.Probe(ctx, dev.key.vid, dev.key.pid)
			if err != nil {
				err = classifyProbeErr(errors.Join(err, context.Cause(ctx)))
			}
			ch <- result{i, inv, err}
		}()
	}
	for ; n > 0; n-- {
		r := <-ch
		switch {
		case errors.Is(r.err, errNotHIDPP):
			out[r.i].status = probeNotHIDPP
		case r.err != nil:
			out[r.i].status, out[r.i].err = probeFailed, r.err
		default:
			out[r.i].status, out[r.i].inv = probeOK, r.inv
		}
	}
	return out
}

// triggerMark flags a --trigger candidate and twinMark a device whose name is
// numbered because another device shares its model: the user checks it against
// the other hosts. Both are wide emoji, so an unmarked row pads to the same
// three cells; variation-selector emoji (⌨️, ✔️) are avoided because
// terminals disagree on their width and the column would tear. A twin keeps
// no trigger mark: its row says only which names to check.
const (
	triggerMark = "✅ "
	twinMark    = "♊ "
	plainMark   = "   "
)

// Layout of the device list: a marker column, the vid:pid, then the name
// padded to the widest name up to nameColMax — a longer product string
// overflows into the tags instead of padding every other row to its width.
// Continuations (pairing slots, scan failures) line up under the name; a
// pairing slot keeps the marker column.
const (
	nameColMax = 24
	contIndent = "              "
	contWidth  = 76 - len(contIndent)
)

// isTrigger reports whether this device can carry "the user switched input to
// this host". renderDevices marks these — a receiver's on their slot lines —
// and triggerTargets lists them, both from here and from isTriggerSlot, so the
// legend cannot drift from the suggestion.
func (d probedDevice) isTrigger() bool {
	if d.key.vid == 0 {
		return false
	}
	// A receiver carries the gesture for the peripherals linked to it, one
	// --trigger entry per pairing slot.
	if d.isReceiver() {
		return len(d.triggerSlots()) > 0
	}
	// A receiver-paired node is not a trigger of its own: hid-logitech-dj
	// keeps it alive for as long as the *pairing* lasts, so it never attaches
	// or detaches when the device moves between hosts (SPEC §6.1). The
	// receiver's link notification is the signal instead.
	if d.shadow {
		return false
	}
	return d.hasKeyboard() || (d.status == probeOK && d.inv.Kind == hidpp.KindMouse)
}

// isReceiver reports whether the probe identified this device as a receiver.
func (d probedDevice) isReceiver() bool {
	return d.status == probeOK && d.inv != nil && d.inv.Kind == hidpp.KindReceiver
}

// triggerSlots returns the pairing slots that can carry the gesture: linked
// right now, and holding a keyboard or a mouse. A slot whose device is paired
// but talking to another host is not offered — its link notification would
// never arrive here.
func (d probedDevice) triggerSlots() []hidpp.PairedDevice {
	if !d.isReceiver() {
		return nil
	}
	var out []hidpp.PairedDevice
	for _, p := range d.inv.Paired {
		if isTriggerSlot(p) {
			out = append(out, p)
		}
	}
	return out
}

func isTriggerSlot(p hidpp.PairedDevice) bool {
	return p.Online && (p.Kind == hidpp.KindKeyboard || p.Kind == hidpp.KindMouse)
}

// tags renders a device row's trailing descriptor: its interface usages,
// deduplicated, then what the probe established. HID++ is claimed only for
// a device that answered one — a failed scan says so on its own line
// rather than tagging the row with a capability it could not confirm.
func (d probedDevice) tags() string {
	names := make([]string, 0, len(d.interfaces)+1)
	for _, u := range d.interfaces {
		if name := usageName(u.usagePage, u.usage); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		names = append(names, "no interfaces")
	}
	switch {
	case d.shadow:
		names = append(names, "via receiver")
	case d.status == probeOK:
		names = append(names, "HID++")
	}
	return strings.Join(names, ", ")
}

// wrapAt greedily wraps text to width columns for a continuation line.
func wrapAt(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if utf8.RuneCountInString(lines[last])+1+utf8.RuneCountInString(word) <= width {
			lines[last] += " " + word
			continue
		}
		lines = append(lines, word)
	}
	return lines
}

func renderDevices(w io.Writer, devices []probedDevice, wiring hostWiring) error {
	if len(devices) == 0 {
		_, err := fmt.Fprintln(w, "No HID devices detected.")
		return err
	}
	nameWidth := 0
	for _, dev := range devices {
		nameWidth = max(nameWidth, len(dev.name()))
	}
	nameWidth = min(nameWidth, nameColMax)
	legend := triggerMark + "= usable as --trigger and connected"
	if len(wiring.twins()) > 0 {
		legend += ", " + twinMark + "= same model as\nanother device: check its name on every host"
	}
	if _, err := fmt.Fprintf(w, "Devices seen (%s):\n\n", legend); err != nil {
		return err
	}
	for _, dev := range devices {
		marker := plainMark
		if dev.isTrigger() && !dev.isReceiver() {
			marker = triggerMark
		}
		if !dev.isReceiver() && wiring.isTwin(hidpp.Target{VID: dev.key.vid, PID: dev.key.pid}) {
			marker = twinMark
		}
		line := fmt.Sprintf("%s%s  %-*s  %s",
			marker, vidpidString(dev.key.vid, dev.key.pid), nameWidth, dev.name(), dev.tags())
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
			return err
		}
		for _, cont := range deviceDetail(dev, wiring) {
			if _, err := fmt.Fprintln(w, cont); err != nil {
				return err
			}
		}
	}
	return nil
}

// deviceDetail is what a device row says on the lines below it: why its HID++
// scan failed, or the receiver's pairing slots, one per line.
func deviceDetail(dev probedDevice, wiring hostWiring) []string {
	switch dev.status {
	case probeFailed:
		lines := wrapAt(dev.scanFailure(), contWidth)
		for i := range lines {
			lines[i] = contIndent + lines[i]
		}
		return lines
	case probeOK:
		return slotLines(dev.inv.Paired, func(slot uint8) bool {
			return wiring.isTwin(hidpp.Target{VID: dev.key.vid, PID: dev.key.pid, Slot: slot})
		})
	default:
		return nil
	}
}

// slotLines renders a receiver's pairing slots: the slot as a declaration
// spells it, the model, the kind, and whether the device is linked to this
// host — a receiver keeps stale pairings for devices that now live on another.
// A linked keyboard or mouse carries the trigger mark, a twin the twin mark.
func slotLines(paired []hidpp.PairedDevice, twin func(slot uint8) bool) []string {
	nameWidth, kindWidth := 0, 0
	for _, p := range paired {
		nameWidth = max(nameWidth, utf8.RuneCountInString(p.Name))
		kindWidth = max(kindWidth, len(p.Kind.String()))
	}
	lines := make([]string, 0, len(paired))
	for _, p := range paired {
		marker, link := plainMark, "not linked"
		if p.Online {
			link = "linked"
		}
		if isTriggerSlot(p) {
			marker = triggerMark
		}
		if twin(p.Index) {
			marker = twinMark
		}
		line := fmt.Sprintf("%s:%d", contIndent[len(plainMark):], p.Index)
		if nameWidth > 0 {
			line += fmt.Sprintf("  %-*s", nameWidth, p.Name)
		}
		lines = append(lines, fmt.Sprintf("%s%s  %-*s  %s", marker, line, kindWidth, p.Kind, link))
	}
	return lines
}

// connectLine renders the suggested connect invocation: the triggers, by
// name, on the first line, then one continuation line per complete
// declaration, and the take commands last. It runs as printed: a declaration
// with a placeholder is left to renderNotes. A device that is no Easy-Switch
// one is declared only to be watched, and left out of a command that does not
// watch it.
func connectLine(triggers []string, w hostWiring) string {
	parts := []string{"soft-kvm connect --trigger " + strings.Join(triggers, ",")}
	for _, d := range w.screens {
		if d.complete() {
			parts = append(parts, declFlag("declare-screen", d.words()))
		}
	}
	for _, d := range w.hids {
		if d.channel != 0 || (d.watched && (d.easySwitch || slices.Contains(triggers, d.name))) {
			parts = append(parts, declFlag("declare-hid", d.words()))
		}
	}
	var takes []string
	if w.takesScreen() {
		takes = append(takes, "-A take-screen")
	}
	if w.takesHID() {
		takes = append(takes, "-A take-hid")
	}
	if len(takes) > 0 {
		parts = append(parts, strings.Join(takes, " "))
	}
	return strings.Join(parts, " \\\n  ")
}

// renderSuggestions prints the invocations as a shell transcript — prose as
// #-comments, commands at column 0 — so the block reads like the one
// `service install` prints and survives a pipe into a file.
func renderSuggestions(w io.Writer, wiring hostWiring) error {
	dim := func(s string) string { return Dim(w, s) }
	kbdTriggers, mouseTriggers := wiring.kbdTriggers, wiring.mouseTriggers

	if len(kbdTriggers) == 0 && len(mouseTriggers) == 0 {
		if _, err := fmt.Fprintln(w, "No trigger candidates detected."); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "Plug the receiver in and re-run detect.")
		return err
	}

	// The declarations are the same either way — the monitor and every
	// peripheral this host reaches — so the two suggestions differ only in what
	// leads the gesture.
	type suggestion struct{ comment, cmd string }
	var suggestions []suggestion
	if len(kbdTriggers) > 0 && wiring.easySwitch() {
		suggestions = append(suggestions, suggestion{
			"# FOLLOW THE KEYBOARD — when the keyboard switches to this computer,\n# bring the monitor and the other devices along.\n",
			connectLine(kbdTriggers, wiring),
		})
	}
	if len(mouseTriggers) > 0 && wiring.easySwitch() {
		suggestions = append(suggestions, suggestion{
			"# FOLLOW THE MOUSE — when the mouse switches to this computer, bring\n# the monitor and the other devices along. Install one, not both.\n",
			connectLine(mouseTriggers, wiring),
		})
	}
	if len(suggestions) == 0 {
		// No HID++ device to declare: trigger-only suggestion.
		triggers := slices.Concat(kbdTriggers, mouseTriggers)
		slices.Sort(triggers)
		triggers = slices.Compact(triggers)
		suggestions = append(suggestions, suggestion{
			"# TAKE OVER ON ATTACH — no HID++ device answered a scan, so only\n# the display follows; the peripherals stay where they are.\n",
			connectLine(triggers, wiring),
		})
	}

	for i, sg := range suggestions {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, dim(sg.comment)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, sg.cmd); err != nil {
			return err
		}
	}
	return nil
}

func (d *hidDevice) name() string {
	switch {
	case d.mfr != "" && d.product != "":
		return d.mfr + " — " + d.product
	case d.mfr != "":
		return d.mfr
	case d.product != "":
		return d.product
	default:
		return "(unknown)"
	}
}

func (d *hidDevice) hasKeyboard() bool {
	for _, u := range d.interfaces {
		if u.usagePage == 0x01 && u.usage == 0x06 {
			return true
		}
	}
	return false
}

// shouldProbe reports whether the device is worth probing for HID++: only
// Logitech devices are, HID++ being their protocol. The vendor ID decides, not
// a vendor-defined interface: other vendors have those too, and Bluetooth
// HID++ peripherals often flatten theirs into the primary HID node. Kernel
// shadows of receiver-paired peripherals are never probed: they accept HID++
// writes but never relay the replies — the device answers through the
// receiver's pairing slots.
func (d *hidDevice) shouldProbe() bool {
	return d.key.vid == logitechVID && !d.shadow
}

func usageName(usagePage, usage uint16) string {
	if usagePage == 0x01 && usage == 0x06 {
		return "keyboard"
	}
	if usagePage == 0x01 && usage == 0x02 {
		return "mouse"
	}
	if usagePage == 0x0C && usage == 0x01 {
		return "consumer"
	}
	return "raw"
}

func vidpidString(vid, pid uint16) string {
	return fmt.Sprintf("%04x:%04x", vid, pid)
}
