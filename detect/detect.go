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
	"strings"
	"time"

	"github.com/telesma-app/hid"
	"github.com/unbrice/soft-kvm/hidpp"
)

const logitechVID = 0x046d

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
}

// probeBudget bounds the whole HID++ scan, all devices together: one shared
// context, so a slow receiver cannot stretch detect linearly.
const probeBudget = 15 * time.Second

// Run enumerates HID devices and prints how to set --trigger for connect.
func Run(ctx context.Context, w io.Writer) error {
	devices, err := enumerateHIDDevices(ctx)
	if err != nil {
		return fmt.Errorf("enumerate HID devices: %w", err)
	}
	probed := probeAll(ctx, devices)
	if err := renderDevices(w, probed); err != nil {
		return err
	}
	if err := renderSuggestions(w, probed); err != nil {
		return err
	}
	return nil
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
	probeSkipped probeStatus = iota
	probeOK                  // inv is set
	probeFailed              // err says why: permission, no answer, ...
)

// The named scan failures, so the display can pick an icon per cause.
var (
	errNoAnswer   = errors.New("no answer")
	errPermission = errors.New("permission denied")
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
		return "🔒 permission denied — a udev rule for hidraw write access is needed (SPEC §5.5)"
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
		if r.err != nil {
			out[r.i].status, out[r.i].err = probeFailed, r.err
		} else {
			out[r.i].status, out[r.i].inv = probeOK, r.inv
		}
	}
	return out
}

func renderDevices(w io.Writer, devices []probedDevice) error {
	if len(devices) == 0 {
		_, err := fmt.Fprintln(w, "No HID devices detected.")
		return err
	}
	type row struct {
		vidpid   string
		name     string
		ifaces   string
		keyboard bool
		hidpp    bool
		dev      probedDevice
	}
	rows := make([]row, len(devices))
	col1Width, col2Width, col3Width := 0, 0, 0
	for i, dev := range devices {
		rows[i] = row{
			vidpid:   vidpidString(dev.key.vid, dev.key.pid),
			name:     dev.name(),
			ifaces:   ifaceList(dev.interfaces),
			keyboard: dev.hasKeyboard(),
			hidpp:    dev.hasHIDPP() || dev.status == probeOK,
			dev:      dev,
		}
		col1Width = max(col1Width, len(rows[i].vidpid))
		col2Width = max(col2Width, len(rows[i].name))
		col3Width = max(col3Width, len(rows[i].ifaces))
	}
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-*s", col1Width, r.vidpid, col2Width, r.name, col3Width, r.ifaces)
		if !r.keyboard {
			line += " (no keyboard)"
		}
		if r.hidpp {
			line += " (HID++)"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		switch r.dev.status {
		case probeFailed:
			if _, err := fmt.Fprintf(w, "  HID++ scan failed: %s\n", r.dev.scanFailure()); err != nil {
				return err
			}
		case probeOK:
			for _, p := range r.dev.inv.Paired {
				if _, err := fmt.Fprintf(w, "  slot %d: %s\n", p.Index, p.Kind); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func renderSuggestions(w io.Writer, devices []probedDevice) error {
	// Trigger candidates are devices whose attach can mean "the user switched
	// input here": keyboard-capable devices (usage page 0x01, usage 0x06) and
	// probed HID++ mice, whose host-switch button is the same gesture.
	var kbdTriggers, mouseTriggers []string
	for _, dev := range devices {
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

	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "Trigger candidates are keyboard-capable devices (usage page 0x01, usage 0x06) and HID++ mice."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(kbdTriggers) == 0 && len(mouseTriggers) == 0 {
		if _, err := fmt.Fprintln(w, "No trigger candidates detected."); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, "Plug the receiver in and re-run detect.")
		return err
	}

	// One suggestion per device that can carry the gesture: the others follow
	// it via hid-switch.
	var labelled [][2]string
	if t := switchTarget(devices, hidpp.KindMouse); len(kbdTriggers) > 0 && t != "" {
		labelled = append(labelled, [2]string{"follow the keyboard",
			"soft-kvm connect --trigger " + strings.Join(kbdTriggers, ",") + " -- " + t})
	}
	if t := switchTarget(devices, hidpp.KindKeyboard); len(mouseTriggers) > 0 && t != "" {
		labelled = append(labelled, [2]string{"follow the mouse",
			"soft-kvm connect --trigger " + strings.Join(mouseTriggers, ",") + " -- " + t})
	}

	if len(labelled) == 0 {
		// No HID++ switch target: trigger-only suggestion.
		triggers := slices.Concat(kbdTriggers, mouseTriggers)
		slices.Sort(triggers)
		if _, err := fmt.Fprintln(w, "Suggested invocation:"); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "  soft-kvm connect --trigger %s\n", strings.Join(triggers, ","))
		return err
	}

	if _, err := fmt.Fprintln(w, "Suggested invocations — choose which device you switch by hand:"); err != nil {
		return err
	}
	width := 0
	for _, l := range labelled {
		width = max(width, len(l[0]))
	}
	for _, l := range labelled {
		if _, err := fmt.Fprintf(w, "  %-*s  %s\n", width, l[0]+":", l[1]); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, `
The trailing hid-switch is a built-in virtual command (SPEC §5.5) that tells
the other peripheral to follow the gesture when this host loses ownership.
Replace <host-index> with the other host's Easy-Switch slot minus one (0-2).
hid-switch moves every paired device of the named kind; to address one
pairing slot instead, give its number (1-6) in place of the kind.`)
	return err
}

// switchTarget renders the hid-switch action that moves devices of the given
// kind: the directly attached device if the probe identified one, else the
// kind form through a receiver that has one paired. "" when no target was
// probed.
func switchTarget(devices []probedDevice, kind hidpp.Kind) string {
	for _, dev := range devices {
		if dev.status == probeOK && dev.inv.Kind == kind {
			return fmt.Sprintf("hid-switch %s <host-index>", vidpidString(dev.key.vid, dev.key.pid))
		}
	}
	for _, dev := range devices {
		if dev.status != probeOK || dev.inv.Kind != hidpp.KindReceiver {
			continue
		}
		for _, p := range dev.inv.Paired {
			if p.Kind == kind {
				return fmt.Sprintf("hid-switch %s %s <host-index>",
					vidpidString(dev.key.vid, dev.key.pid), kind)
			}
		}
	}
	return ""
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

func ifaceList(ifaces []ifaceUsage) string {
	n := len(ifaces)
	if n == 0 {
		return "0 interfaces"
	}
	names := make([]string, n)
	for i, u := range ifaces {
		names[i] = usageName(u.usagePage, u.usage)
	}
	noun := "interfaces"
	if n == 1 {
		noun = "interface"
	}
	return fmt.Sprintf("%d %s: %s", n, noun, strings.Join(names, ", "))
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
func (d *hidDevice) shouldProbe() bool {
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
