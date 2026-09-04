// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// detect.go: HID device enumeration, --trigger suggestions, and the
// follow-keyboard/switch-mouse hid-switch suggestion (SPEC §5.5, §6.1).

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

// Run enumerates HID devices and prints how to set --trigger for connect:
// the suggested invocations first, as a shell transcript, then the device
// list they were drawn from.
func Run(ctx context.Context, w io.Writer) error {
	devices, err := enumerateHIDDevices(ctx)
	if err != nil {
		return fmt.Errorf("enumerate HID devices: %w", err)
	}
	probed := probeAll(ctx, devices)
	if err := renderSuggestions(w, probed); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return renderDevices(w, probed)
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
// probeSkipped, for devices with no vendor interface that were never
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

// scanFailure renders a failed scan for display. A permission fix is only
// worth spelling out for Logitech devices: HID++ is their protocol, so a
// denied scan anywhere else found nothing and needs no action.
func (d probedDevice) scanFailure() string {
	switch {
	case errors.Is(d.err, errPermission) && d.key.vid == logitechVID:
		return "🔒 HID++ scan denied — " + PermissionRemediation
	case errors.Is(d.err, errPermission):
		return "🔒 HID++ scan denied; not a Logitech device, no HID++ to find"
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

// probeAll scans HID++-capable devices in parallel and returns every device,
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

// triggerMark flags a --trigger candidate. It is a wide emoji, so an
// unmarked row pads to the same three cells; variation-selector emoji
// (⌨️, ✔️) are avoided because terminals disagree on their width and the
// column would tear.
const (
	triggerMark = "✅ "
	plainMark   = "   "
)

// Layout of the device list: a marker column, the vid:pid, then the name
// padded to the widest name up to nameColMax — a longer product string
// overflows into the tags instead of padding every other row to its width.
// Continuations (pairing slots, scan failures) line up under the name.
const (
	nameColMax = 24
	contIndent = "              "
	contWidth  = 76 - len(contIndent)
)

// isTrigger reports whether this device can carry "the user switched input to
// this host". renderDevices marks these and renderSuggestions lists them, both
// from here, so the legend cannot drift from the suggestion.
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
		if p.Online && (p.Kind == hidpp.KindKeyboard || p.Kind == hidpp.KindMouse) {
			out = append(out, p)
		}
	}
	return out
}

// triggerString renders one --trigger entry for a receiver pairing slot.
func triggerString(vid, pid uint16, slot uint8) string {
	return fmt.Sprintf("%s:%d", vidpidString(vid, pid), slot)
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

func renderDevices(w io.Writer, devices []probedDevice) error {
	if len(devices) == 0 {
		_, err := fmt.Fprintln(w, "No HID devices detected.")
		return err
	}
	nameWidth := 0
	for _, dev := range devices {
		nameWidth = max(nameWidth, len(dev.name()))
	}
	nameWidth = min(nameWidth, nameColMax)
	if _, err := fmt.Fprintf(w, "Devices seen (%s= usable as --trigger and connected):\n\n", triggerMark); err != nil {
		return err
	}
	for _, dev := range devices {
		marker := plainMark
		if dev.isTrigger() {
			marker = triggerMark
		}
		line := fmt.Sprintf("%s%s  %-*s  %s",
			marker, vidpidString(dev.key.vid, dev.key.pid), nameWidth, dev.name(), dev.tags())
		if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
			return err
		}
		for _, cont := range deviceDetail(dev) {
			if _, err := fmt.Fprintln(w, contIndent+cont); err != nil {
				return err
			}
		}
	}
	return nil
}

// deviceDetail is what a device row says on its continuation lines: the
// receiver's pairing slots, or why its HID++ scan failed.
func deviceDetail(dev probedDevice) []string {
	switch dev.status {
	case probeFailed:
		return wrapAt(dev.scanFailure(), contWidth)
	case probeOK:
		if len(dev.inv.Paired) == 0 {
			return nil
		}
		// Linked slots carry their kind — they are the ones worth naming in a
		// --trigger. The rest collapse into a trailing group: a receiver keeps
		// stale pairings for devices that now live on another host.
		var linked, idle []string
		for _, p := range dev.inv.Paired {
			if p.Online {
				linked = append(linked, fmt.Sprintf("%d %s", p.Index, p.Kind))
			} else {
				idle = append(idle, strconv.Itoa(int(p.Index)))
			}
		}
		var parts []string
		if len(linked) > 0 {
			parts = append(parts, strings.Join(linked, ", ")+" linked")
		}
		if len(idle) > 0 {
			parts = append(parts, strings.Join(idle, ", ")+" not linked")
		}
		return wrapAt("paired: "+strings.Join(parts, "; "), contWidth)
	default:
		return nil
	}
}

// cmdWidth is the column budget a suggested command must fit, before it is
// broken across a shell continuation.
const cmdWidth = 76

// wrapCommand keeps a suggestion inside the budget: the one-line form when
// it fits, else a continuation before the -- that introduces the switch
// command.
func wrapCommand(cmd string) string {
	if utf8.RuneCountInString(cmd) <= cmdWidth {
		return cmd
	}
	if head, tail, ok := strings.Cut(cmd, " -- "); ok {
		return head + " \\\n  -- " + tail
	}
	return cmd
}

// renderSuggestions prints the invocations as a shell transcript — prose as
// #-comments, commands at column 0 — so the block reads like the one
// `service install` prints and survives a pipe into a file.
func renderSuggestions(w io.Writer, devices []probedDevice) error {
	dim := func(s string) string { return Dim(w, s) }
	var kbdTriggers, mouseTriggers []string
	for _, dev := range devices {
		if !dev.isTrigger() {
			continue
		}
		// A receiver is addressed per pairing slot; everything else by its
		// own VID:PID, whose HID node really does come and go.
		if dev.isReceiver() {
			for _, p := range dev.triggerSlots() {
				t := triggerString(dev.key.vid, dev.key.pid, p.Index)
				switch p.Kind {
				case hidpp.KindKeyboard:
					kbdTriggers = append(kbdTriggers, t)
				case hidpp.KindMouse:
					mouseTriggers = append(mouseTriggers, t)
				}
			}
			continue
		}
		vidpid := vidpidString(dev.key.vid, dev.key.pid)
		if dev.hasKeyboard() {
			kbdTriggers = append(kbdTriggers, vidpid)
		}
		if dev.status == probeOK && dev.inv.Kind == hidpp.KindMouse {
			mouseTriggers = append(mouseTriggers, vidpid)
		}
	}
	// Zero-padded %04x:%04x sorts lexicographically; sort here rather than
	// rely on enumeration order.
	slices.Sort(kbdTriggers)
	slices.Sort(mouseTriggers)

	if len(kbdTriggers) == 0 && len(mouseTriggers) == 0 {
		if _, err := fmt.Fprintln(w, "No trigger candidates detected."); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "Plug the receiver in and re-run detect.")
		return err
	}

	// The action is the same either way — move whatever is still linked here
	// — so the two suggestions differ only in what leads the gesture.
	action := ""
	if cmds := switchCommands(devices); len(cmds) > 0 {
		action = " -- " + strings.Join(cmds, " -- ")
	}
	type suggestion struct{ comment, cmd string }
	var suggestions []suggestion
	if len(kbdTriggers) > 0 && action != "" {
		suggestions = append(suggestions, suggestion{
			"# FOLLOW THE KEYBOARD — switch when the keyboard takes the link\n# here, and send the other peripherals along.\n",
			"soft-kvm connect --trigger " + strings.Join(kbdTriggers, ",") + action,
		})
	}
	if len(mouseTriggers) > 0 && action != "" {
		suggestions = append(suggestions, suggestion{
			"# FOLLOW THE MOUSE — switch when the mouse takes the link here,\n# and send the other peripherals along. Install one, not both.\n",
			"soft-kvm connect --trigger " + strings.Join(mouseTriggers, ",") + action,
		})
	}
	if len(suggestions) == 0 {
		// No HID++ switch target: trigger-only suggestion.
		triggers := slices.Concat(kbdTriggers, mouseTriggers)
		slices.Sort(triggers)
		suggestions = append(suggestions, suggestion{
			"# SWITCH ON ATTACH — no HID++ device answered a scan, so only\n# the display follows; the peripherals stay where they are.\n",
			"soft-kvm connect --trigger " + strings.Join(triggers, ","),
		})
	}

	usesHIDSwitch := false
	for i, sg := range suggestions {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, dim(sg.comment)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, wrapCommand(sg.cmd)); err != nil {
			return err
		}
		usesHIDSwitch = usesHIDSwitch || strings.Contains(sg.cmd, "hid-switch")
	}
	if !usesHIDSwitch {
		return nil
	}
	_, err := io.WriteString(w, dim(`
# hid-switch is built in, not a program. Named without a slot it moves
# every peripheral still linked to that device — the one that carried
# the gesture has already left, so it is skipped. The target channel
# comes from the winning host, which publishes its own with its claim.
# Append host=N (1-3, as printed on the key) to pin it, or address one
# peripheral as VID:PID:SLOT.
`))
	return err
}

// switchCommands renders the hid-switch actions the losing host runs: one per
// local device that can carry a peripheral. Each moves whatever is still
// linked to it, so the same list serves whichever peripheral led the gesture
// — the one that led has already left, and its entry is a no-op. Keyboard and
// mouse often sit on different receivers, so this is a list, not one command.
func switchCommands(devices []probedDevice) []string {
	var out []string
	for _, dev := range devices {
		if dev.status != probeOK || dev.inv == nil {
			continue
		}
		switch dev.inv.Kind {
		case hidpp.KindReceiver:
			if len(dev.inv.Paired) == 0 {
				continue
			}
		case hidpp.KindKeyboard, hidpp.KindMouse:
		default:
			continue
		}
		out = append(out, "hid-switch "+vidpidString(dev.key.vid, dev.key.pid))
	}
	slices.Sort(out)
	return slices.Compact(out)
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

// hasHIDPP reports whether the device carries a vendor-defined interface
// (usage page ≥0xFF00), where Logitech typically hides HID++.
func (d *hidDevice) hasHIDPP() bool {
	for _, u := range d.interfaces {
		if u.usagePage >= 0xFF00 {
			return true
		}
	}
	return false
}

// shouldProbe reports whether the device is worth probing for HID++: it either
// exposes a vendor-defined interface or is a Logitech device (Bluetooth HID++
// peripherals often flatten the vendor collection into the primary HID node).
// Kernel shadows of receiver-paired peripherals are never probed: they accept
// HID++ writes but never relay the replies — the device answers through the
// receiver's pairing slots.
func (d *hidDevice) shouldProbe() bool {
	if d.key.vid == 0 || d.shadow {
		return false
	}
	return d.hasHIDPP() || d.key.vid == logitechVID
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
