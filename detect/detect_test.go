// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Tests for detect.go rendering and suggestion logic.

package detect

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/unbrice/soft-kvm/hidpp"
	"github.com/unbrice/soft-kvm/screen"
)

func TestUsageName(t *testing.T) {
	cases := []struct {
		page, usage uint16
		want        string
	}{
		{0x01, 0x06, "keyboard"},
		{0x01, 0x02, "mouse"},
		{0x0C, 0x01, "consumer"},
		{0x01, 0x04, "raw"},
		{0xFF00, 0x01, "raw"},
	}
	for _, c := range cases {
		got := usageName(c.page, c.usage)
		if got != c.want {
			t.Errorf("usageName(0x%04x, 0x%04x) = %q, want %q", c.page, c.usage, got, c.want)
		}
	}
}

func TestDeviceName(t *testing.T) {
	cases := []struct {
		mfr, product string
		want         string
	}{
		{"Logitech", "USB Receiver", "Logitech — USB Receiver"},
		{"", "Product", "Product"},
		{"Mfr", "", "Mfr"},
		{"", "", "(unknown)"},
	}
	for _, c := range cases {
		dev := &hidDevice{mfr: c.mfr, product: c.product}
		got := dev.name()
		if got != c.want {
			t.Errorf("name(%q, %q) = %q, want %q", c.mfr, c.product, got, c.want)
		}
	}
}

func TestHasKeyboard(t *testing.T) {
	withKB := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x06}, {0x01, 0x02}}}
	withoutKB := &hidDevice{interfaces: []ifaceUsage{{0x01, 0x02}}}
	if !withKB.hasKeyboard() {
		t.Error("expected keyboard")
	}
	if withoutKB.hasKeyboard() {
		t.Error("expected no keyboard")
	}
}

func TestRenderDevices(t *testing.T) {
	devices := []probedDevice{
		{
			hidDevice: hidDevice{
				key:        deviceKey{vid: 0x046d, pid: 0xc548},
				mfr:        "Logitech",
				product:    "USB Receiver",
				interfaces: []ifaceUsage{{0x01, 0x06}, {0x01, 0x02}, {0xFF00, 0x01}},
			},
			status: probeOK,
			inv: &hidpp.Inventory{
				Kind: hidpp.KindReceiver,
				Paired: []hidpp.PairedDevice{
					{Index: 1, Kind: hidpp.KindKeyboard, Name: "ERGO K860", Online: true},
					{Index: 2, Kind: hidpp.KindMouse, Name: "LIFT"},
				},
			},
		},
		{
			hidDevice: hidDevice{
				key:        deviceKey{vid: 0x046d, pid: 0xb023},
				mfr:        "Logitech",
				product:    "MX Master 3",
				interfaces: []ifaceUsage{{0x01, 0x02}, {0xFF00, 0x01}},
			},
			status: probeFailed,
			err:    errPermission,
		},
	}
	var buf bytes.Buffer
	if err := renderDevices(&buf, devices, hostWiring{}); err != nil {
		t.Fatalf("renderDevices: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "046d:c548") {
		t.Error("missing receiver")
	}
	if !strings.Contains(out, "keyboard, mouse, raw, HID++") {
		t.Error("missing interface list and HID++ tag")
	}
	// Slot 1 holds the link and can be a trigger; slot 2 is a stale pairing
	// whose device is talking to another host. The receiver itself is no
	// trigger: --trigger names a device.
	slots := "keyboard, mouse, raw, HID++\n" +
		triggerMark + "           :1  ERGO K860  keyboard  linked\n" +
		"              :2  LIFT       mouse     not linked\n"
	if !strings.Contains(out, plainMark+"046d:c548") || !strings.Contains(out, slots) {
		t.Errorf("missing receiver slot map with link state %q in:\n%s", slots, out)
	}
	if !strings.Contains(out, "🔒 HID++ scan denied") {
		t.Error("missing scan failure reason")
	}
	// The denied mouse has no keyboard interface and no successful probe:
	// it is not a candidate, and a failed scan must not tag it HID++.
	for line := range strings.Lines(out) {
		if !strings.Contains(line, "046d:b023") {
			continue
		}
		if strings.HasPrefix(line, triggerMark) {
			t.Errorf("non-candidate marked as a trigger: %q", line)
		}
		if strings.Contains(line, "HID++") {
			t.Errorf("HID++ claimed for a device whose scan failed: %q", line)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line is %d columns, it wraps in an 80-column terminal: %q", n, line)
		}
	}
}

func TestRenderDevicesProbeOKWithoutVendor(t *testing.T) {
	// A Bluetooth HID++ mouse may flatten the vendor collection into the
	// primary mouse node; a successful probe should still show the HID++ marker.
	devices := []probedDevice{
		{
			hidDevice: hidDevice{
				key:        deviceKey{vid: 0x046d, pid: 0xb036},
				mfr:        "Logitech",
				product:    "Pebble M350s",
				interfaces: []ifaceUsage{{0x01, 0x02}},
			},
			status: probeOK,
			inv:    &hidpp.Inventory{Kind: hidpp.KindMouse},
		},
	}
	var buf bytes.Buffer
	if err := renderDevices(&buf, devices, hostWiring{}); err != nil {
		t.Fatalf("renderDevices: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "mouse, HID++") {
		t.Errorf("missing HID++ marker for probeOK device without vendor interface:\n%s", out)
	}
}

// noScreenWiring is what detect declares for devices when no monitor answers
// and no channel was read.
func noScreenWiring(devices ...probedDevice) hostWiring {
	return buildWiring(devices, screen.Report{}, nil)
}

func TestRenderSuggestions(t *testing.T) {
	devices := []probedDevice{
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: 0xc548},
			mfr:        "Logitech",
			product:    "USB Receiver",
			interfaces: []ifaceUsage{{0x01, 0x06}},
		}},
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: 0xb35b},
			mfr:        "Logitech",
			product:    "MX Keys",
			interfaces: []ifaceUsage{{0x01, 0x06}},
		}},
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}},
		}},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, noScreenWiring(devices...)); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	// The command runs as printed: the screen, still a placeholder, is left
	// to the notes. A trigger is a name, so each one is declared, with no
	// channel: these are watched, never taken.
	if !strings.Contains(out, "soft-kvm connect --trigger mx-keys,usb-receiver \\\n"+
		"  --declare-hid 'mx-keys 046d:b35b' \\\n"+
		"  --declare-hid 'usb-receiver 046d:c548'\n") {
		t.Errorf("missing suggestion listing the keyboard-capable devices only, sorted:\n%s", out)
	}
	if strings.Contains(out, "DP1_HDMI_ETC") || strings.Contains(out, "take-screen") {
		t.Errorf("a placeholder declaration in the command:\n%s", out)
	}
	if strings.Contains(out, "1532:0285") {
		t.Error("non-keyboard device leaked into the suggestion")
	}
	if strings.Contains(out, "take-hid") || strings.Contains(out, "host=") {
		t.Error("a device no scan answered cannot be taken")
	}
}

func TestRenderSuggestionsFiltersVIDZero(t *testing.T) {
	internal := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0, pid: 0},
		interfaces: []ifaceUsage{{0x01, 0x06}},
	}}
	external := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0x046d, pid: 0xb359},
		interfaces: []ifaceUsage{{0x01, 0x06}},
	}}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, noScreenWiring(internal, external)); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "0000:0000") {
		t.Errorf("expected internal device 0000:0000 to be filtered from suggestions:\n%s", out)
	}
	if !strings.Contains(out, "046d:b359") {
		t.Errorf("expected external keyboard 046d:b359 to be in suggestions:\n%s", out)
	}
}

func probedReceiver(pid uint16, paired ...hidpp.PairedDevice) probedDevice {
	return probedDevice{
		hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: pid},
			mfr:        "Logitech",
			product:    "USB Receiver",
			interfaces: []ifaceUsage{{0x01, 0x06}, {0xFF43, 0x02}},
		},
		status: probeOK,
		inv:    &hidpp.Inventory{Kind: hidpp.KindReceiver, Paired: paired},
	}
}

func probedDirect(pid uint16, kind hidpp.Kind) probedDevice {
	usage, product := ifaceUsage{0x01, 0x06}, "MX Keys"
	if kind == hidpp.KindMouse {
		usage, product = ifaceUsage{0x01, 0x02}, "MX Master 3S"
	}
	return probedDevice{
		hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: pid},
			mfr:        "Logitech",
			product:    product,
			interfaces: []ifaceUsage{usage, {0xFF43, 0x02}},
		},
		status: probeOK,
		inv:    &hidpp.Inventory{Kind: kind},
	}
}

func TestBuildWiringDevices(t *testing.T) {
	slot := func(vid, pid uint16, s uint8) hidpp.Target { return hidpp.Target{VID: vid, PID: pid, Slot: s} }
	receiver := probedReceiver(0xc548,
		hidpp.PairedDevice{Index: 1, Kind: hidpp.KindKeyboard, Name: "ERGO K860", Online: true},
		hidpp.PairedDevice{Index: 2, Kind: hidpp.KindMouse, Name: "MX Master 3S"},
		hidpp.PairedDevice{Index: 3, Kind: hidpp.KindMouse, Online: true},
		hidpp.PairedDevice{Index: 4, Kind: hidpp.KindUnknown, Name: "Presenter"},
	)
	direct := probedDirect(0xb034, hidpp.KindMouse)
	shadow := probedDirect(0x4088, hidpp.KindKeyboard)
	shadow.shadow = true
	other := probedDirect(0xb35b, hidpp.KindKeyboard)
	other.key.vid = 0x1532
	unprobed := probedDevice{hidDevice: hidDevice{key: deviceKey{vid: 0x046d, pid: 0xb35b}, interfaces: []ifaceUsage{{0x01, 0x06}}}}

	channels := map[hidpp.Target]uint8{slot(0x046d, 0xc548, 1): 2, slot(0x046d, 0xc548, 3): 3, {VID: 0x046d, PID: 0xb034}: 1}
	w := buildWiring([]probedDevice{receiver, direct, shadow, unprobed}, screen.Report{}, channels)

	want := []hidDecl{
		// Sorted by address. A receiver is declared per slot, linked or not;
		// the channel is known only where the device is linked to this host.
		// A device is named after its model, after its kind when it has none,
		// and the second of a model, by address, is numbered, both of the
		// pair flagged. A trigger no scan answered is declared to be watched.
		{name: "mx-master-3s", target: hidpp.Target{VID: 0x046d, PID: 0xb034}, channel: 1, easySwitch: true, twin: true},
		{name: "keyboard", target: hidpp.Target{VID: 0x046d, PID: 0xb35b}, watched: true},
		{name: "ergo-k860", target: slot(0x046d, 0xc548, 1), channel: 2, easySwitch: true},
		{name: "mx-master-3s-2", target: slot(0x046d, 0xc548, 2), easySwitch: true, twin: true},
		{name: "mouse", target: slot(0x046d, 0xc548, 3), channel: 3, easySwitch: true},
	}
	if !slices.Equal(w.hids, want) {
		t.Fatalf("hids = %+v\nwant   %+v", w.hids, want)
	}
	wantWords := [][]string{
		{"mx-master-3s", "046d:b034", "host=1"},
		{"keyboard", "046d:b35b"},
		{"ergo-k860", "046d:c548:1", "host=2"},
		{"mx-master-3s-2", "046d:c548:2", "host=N"},
		{"mouse", "046d:c548:3", "host=3"},
	}
	for i, d := range w.hids {
		if !slices.Equal(d.words(), wantWords[i]) {
			t.Errorf("hid %d words = %q, want %q", i, d.words(), wantWords[i])
		}
	}
	if got := w.pendingHIDs(); len(got) != 1 || got[0].name != "mx-master-3s-2" {
		t.Errorf("pending = %+v, want only the device not linked here", got)
	}
	if want := []string{"ergo-k860", "keyboard"}; !slices.Equal(w.kbdTriggers, want) {
		t.Errorf("kbdTriggers = %q, want %q", w.kbdTriggers, want)
	}
	if want := []string{"mouse", "mx-master-3s"}; !slices.Equal(w.mouseTriggers, want) {
		t.Errorf("mouseTriggers = %q, want %q", w.mouseTriggers, want)
	}
}

func TestDeclName(t *testing.T) {
	cases := []struct{ model, want string }{
		{"MX Master 3S", "mx-master-3s"},
		{"ERGO K860", "ergo-k860"},
		{"LIFT", "lift"},
		{"Logitech MX Keys", "mx-keys"},
		{"  M720 (Triathlon)  ", "m720-triathlon"},
		{"", "mouse"},
		{"é", "mouse"},
		{strings.Repeat("x", 65), "mouse"},
	}
	for _, c := range cases {
		if got := declName(c.model, hidpp.KindMouse); got != c.want {
			t.Errorf("declName(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

// What detect prints must be what connect accepts: every declaration parses
// with the real parsers, spelled as the flag value connect splits.
func TestDeclarationsParse(t *testing.T) {
	r := screen.Report{Backend: "ddcutil", Displays: []screen.Display{
		{Mfg: "GSM", Model: `LG "HDR" WQHD`, Input: 0x0F, InputOK: true},
		{Mfg: "DEL", Model: "DELL U2720Q"},
	}}
	receiver := probedReceiver(0xc548, hidpp.PairedDevice{Index: 2, Kind: hidpp.KindMouse, Online: true})
	w := buildWiring([]probedDevice{receiver}, r, map[hidpp.Target]uint8{{VID: 0x046d, PID: 0xc548, Slot: 2}: 1})
	for _, d := range w.screens {
		words := d.words()
		// connect splits the value with the same word grammar; check the
		// quoting by reading the words back.
		if got, err := screen.ParseDeclaration(unquoteWords(t, strings.Join(words, " "))); err != nil && d.input != inputPlaceholder {
			t.Errorf("screen %+v: %v (%q)", d, err, got)
		}
	}
	if got, err := screen.ParseDeclaration(unquoteWords(t, strings.Join(w.screens[0].words(), " "))); err != nil ||
		got.Name != `LG "HDR" WQHD` || got.Write.String() != "F4:50=D0" {
		t.Errorf("first screen = %+v, %v; want the LG on dp1 through its table", got, err)
	}
	for _, d := range w.hids {
		if _, err := hidpp.ParseDeclaration(d.words()); err != nil {
			t.Errorf("hid %+v: %v", d, err)
		}
	}
}

// unquoteWords splits s the way connect does: whitespace separates, double
// quotes group, a backslash escapes a quote or a backslash inside them.
func unquoteWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inQuote, inWord := false, false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case inQuote && r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
		case r == '"':
			inQuote, inWord = !inQuote, true
		case r == ' ' && !inQuote:
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func TestRenderSuggestionsDeclarations(t *testing.T) {
	keyboard := probedDirect(0xb35b, hidpp.KindKeyboard)
	mouse := probedDirect(0xb034, hidpp.KindMouse)
	r := screen.Report{Backend: "ddcutil", Displays: []screen.Display{
		{Mfg: "GSM", Model: "LG HDR WQHD", Input: 0x0F, InputOK: true},
	}}
	w := buildWiring([]probedDevice{keyboard, mouse}, r, map[hidpp.Target]uint8{
		{VID: 0x046d, PID: 0xb35b}: 1, {VID: 0x046d, PID: 0xb034}: 2,
	})
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, w); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()

	// Both gestures are suggested, each trigger by the name the command
	// declares it under, and both carry the same declarations.
	for _, trigger := range []string{"mx-keys", "mx-master-3s"} {
		cmd := "soft-kvm connect --trigger " + trigger + " \\\n" +
			"  --declare-screen '\"LG HDR WQHD\" dp1 lg' \\\n" +
			"  --declare-hid 'mx-master-3s 046d:b034 host=2' \\\n" +
			"  --declare-hid 'mx-keys 046d:b35b host=1' \\\n" +
			"  -A take-screen -A take-hid\n"
		if !strings.Contains(out, cmd) {
			t.Errorf("missing the suggestion for %s:\n%s", trigger, out)
		}
	}
	if !strings.Contains(out, "# FOLLOW THE KEYBOARD") || !strings.Contains(out, "# FOLLOW THE MOUSE") {
		t.Errorf("missing a gesture:\n%s", out)
	}
	if strings.Contains(out, "DP1_HDMI_ETC") || strings.Contains(out, "host=N") {
		t.Errorf("a placeholder survives where the host knows the answer:\n%s", out)
	}
	assertTranscript(t, out)
}

// assertTranscript pins the shell-transcript shape: every line is a #-comment,
// a command at column 0, a continuation of a \-terminated command, or blank,
// and no command holds a placeholder.
func assertTranscript(t *testing.T, out string) {
	t.Helper()
	prev := ""
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "", strings.HasPrefix(line, "#"):
			prev = line
			continue
		case !strings.HasPrefix(line, " "):
		case strings.HasSuffix(prev, `\`):
		default:
			t.Errorf("line is neither comment, command nor continuation: %q", line)
		}
		// A command runs as printed: placeholders live in comments.
		if strings.Contains(line, inputPlaceholder) || strings.Contains(line, channelPlaceholder) {
			t.Errorf("placeholder in a command line: %q", line)
		}
		prev = line
	}
}

func TestRenderSuggestionsPlaceholders(t *testing.T) {
	// A receiver slot the other host holds, and a monitor this host is not on:
	// neither the channel nor the port can be read here.
	receiver := probedReceiver(0xc548, hidpp.PairedDevice{Index: 1, Kind: hidpp.KindKeyboard, Name: "MX Keys", Online: true},
		hidpp.PairedDevice{Index: 2, Kind: hidpp.KindMouse, Name: "LIFT"})
	r := screen.Report{Backend: "ddcutil", Displays: []screen.Display{{Mfg: "DEL", Model: "DELL U2720Q"}}}
	w := buildWiring([]probedDevice{receiver}, r, map[hidpp.Target]uint8{{VID: 0x046d, PID: 0xc548, Slot: 1}: 1})
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, w); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if err := renderNotes(&buf, r, w); err != nil {
		t.Fatalf("renderNotes: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		// The command holds what is complete and runs as printed.
		"soft-kvm connect --trigger mx-keys \\\n" +
			"  --declare-hid 'mx-keys 046d:c548:1 host=1' \\\n" +
			"  -A take-hid\n",
		// The rest is a comment to complete, with the take it needs.
		"\n# ⚠️ Add these flags for the monitor to move as well:\n" +
			"#   --declare-screen '\"DELL U2720Q\" DP1_HDMI_ETC'\n" +
			"#   -A take-screen\n",
		"\n# ⚠️ Add flags for the following devices if you want them to move as well:\n" +
			"#   --declare-hid 'lift 046d:c548:2 host=N'\n" +
			"# Replace host=N",
		"# Replace DP1_HDMI_ETC with this computer's port on the monitor (dp1, dp2,\n" +
			"# hdmi1, hdmi2, usbc1, or a raw value such as 0x1B), or switch the monitor\n" +
			"# to this host and run detect again.\n",
		"# Replace host=N with this computer's number on the device, or switch the\n" +
			"# device to this host and run detect again.\n",
		"\n# Device names must be the same on every host.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Another host's detect prints that host's port and channel: never send
	// the user there.
	for _, bad := range []string{"from there", "host the device is on"} {
		if strings.Contains(out, bad) {
			t.Errorf("note points at another host (%q):\n%s", bad, out)
		}
	}
	assertTranscript(t, out)
}

func TestRenderTwins(t *testing.T) {
	// Two mice of one model: numbered by address here, in an order another
	// host need not share. The notes and the list both say to check them.
	receiver := probedReceiver(0xc548,
		hidpp.PairedDevice{Index: 1, Kind: hidpp.KindKeyboard, Name: "MX Keys", Online: true},
		hidpp.PairedDevice{Index: 2, Kind: hidpp.KindMouse, Name: "MX Master 3S", Online: true},
		hidpp.PairedDevice{Index: 3, Kind: hidpp.KindMouse, Name: "MX Master 3S"})
	devices := []probedDevice{receiver}
	channels := map[hidpp.Target]uint8{{VID: 0x046d, PID: 0xc548, Slot: 1}: 1}
	w := buildWiring(devices, screen.Report{}, channels)
	if want := []string{"mx-master-3s", "mx-master-3s-2"}; !slices.Equal(w.twins(), want) {
		t.Fatalf("twins = %q, want %q", w.twins(), want)
	}
	var buf bytes.Buffer
	if err := renderNotes(&buf, screen.Report{}, w); err != nil {
		t.Fatalf("renderNotes: %v", err)
	}
	want := "\n# " + twinMark + "Device names must be the same on every host.\n" +
		"# Check that mx-master-3s and mx-master-3s-2 name the same devices on\n" +
		"# every host: detect numbers devices of one model by address, and another\n" +
		"# host may order them differently.\n" +
		"# " + warnSign + "A name that means a different device there makes take-hid move the\n" +
		"#    wrong one.\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("missing %q in:\n%s", want, buf.String())
	}
	buf.Reset()
	if err := renderDevices(&buf, devices, w); err != nil {
		t.Fatalf("renderDevices: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Devices seen (" + triggerMark + "= usable as --trigger and connected, " + twinMark + "= same model as\n" +
			"another device: check its name on every host):\n",
		// A linked twin is a trigger too: the twin mark wins. An unlinked one
		// is marked all the same, its name is declared.
		triggerMark + "           :1  MX Keys",
		twinMark + "           :2  MX Master 3S  mouse     linked\n",
		twinMark + "           :3  MX Master 3S  mouse     not linked\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Without a twin there is no mark and no legend entry for it.
	buf.Reset()
	if err := renderDevices(&buf, devices, hostWiring{}); err != nil {
		t.Fatalf("renderDevices: %v", err)
	}
	if strings.Contains(buf.String(), twinMark) {
		t.Errorf("twin mark without a twin:\n%s", buf.String())
	}
}

func TestRenderSuggestionsReceiverOnlyTriggers(t *testing.T) {
	keyboard := probedDirect(0xb35b, hidpp.KindKeyboard)
	// A failed probe declares nothing to take — its keyboard interface still
	// makes it a trigger to watch — and a receiver with nothing paired has
	// nothing to declare at all.
	failed := probedReceiver(0xc548)
	failed.status = probeFailed
	failed.inv = nil
	failed.err = fs.ErrPermission
	empty := probedReceiver(0xc52b)
	w := buildWiring([]probedDevice{failed}, screen.Report{}, nil)
	if want := []hidDecl{{name: "usb-receiver", target: hidpp.Target{VID: 0x046d, PID: 0xc548}, watched: true}}; !slices.Equal(w.hids, want) {
		t.Errorf("failed receiver: declared %+v, want %+v", w.hids, want)
	}
	if got := buildWiring([]probedDevice{empty}, screen.Report{}, nil).hids; len(got) != 0 {
		t.Errorf("empty receiver: declared %+v", got)
	}
	// Without any device to declare, the suggestion is the attach-only one.
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, noScreenWiring(probedDevice{hidDevice: keyboard.hidDevice}, empty)); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "# TAKE OVER ON ATTACH") || strings.Contains(out, "take-hid") {
		t.Errorf("want the attach-only suggestion:\n%s", out)
	}
}

func TestShouldProbe(t *testing.T) {
	vendor := []ifaceUsage{{0x01, 0x06}, {0xFF43, 0x02}}
	cases := []struct {
		name string
		dev  hidDevice
		want bool
	}{
		{"logitech", hidDevice{key: deviceKey{vid: logitechVID}, interfaces: vendor}, true},
		{"logitech without vendor interface", hidDevice{key: deviceKey{vid: logitechVID}}, true},
		{"logitech shadow", hidDevice{key: deviceKey{vid: logitechVID}, shadow: true}, false},
		{"other vendor with vendor interface", hidDevice{key: deviceKey{vid: 0x1532}, interfaces: vendor}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.dev.shouldProbe(); got != c.want {
				t.Errorf("shouldProbe() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRenderSuggestionsNoCandidates(t *testing.T) {
	devices := []probedDevice{
		{hidDevice: hidDevice{
			key:        deviceKey{vid: 0x1532, pid: 0x0285},
			mfr:        "Razer",
			product:    "DeathAdder V2",
			interfaces: []ifaceUsage{{0x01, 0x02}},
		}},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, noScreenWiring(devices...)); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No trigger candidates detected") {
		t.Error("missing no-candidates message")
	}
	if strings.Contains(out, "Suggested invocation") {
		t.Error("unexpected suggestion when no candidates")
	}
}

func TestProbeAllSkipsNonLogitech(t *testing.T) {
	// Not Logitech, vendor interface or not: no goroutine, no hardware touched —
	// every device comes back probeSkipped, in order.
	devices := []*hidDevice{
		{key: deviceKey{vid: 0x1532, pid: 0x0285}, interfaces: []ifaceUsage{{0x01, 0x02}, {0xFF00, 0x01}}},
		{key: deviceKey{vid: 0x1050, pid: 0x0407}, interfaces: []ifaceUsage{{0x01, 0x06}}},
	}
	out := probeAll(t.Context(), devices)
	if len(out) != len(devices) {
		t.Fatalf("probeAll returned %d devices, want %d", len(out), len(devices))
	}
	for i, d := range out {
		if d.key != devices[i].key {
			t.Errorf("device %d: key = %v, want %v (order changed)", i, d.key, devices[i].key)
		}
		if d.status != probeSkipped {
			t.Errorf("device %d: status = %d, want probeSkipped", i, d.status)
		}
	}
}

func TestScanFailure(t *testing.T) {
	cases := []struct {
		name string
		vid  uint16
		err  error
		want string
	}{
		{"permission", logitechVID, errPermission, permissionRemediation},
		{"no answer", 0, errNoAnswer, "⏳ no answer"},
		{"other", 0, errors.New("boom"), "⚠️ boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := probedDevice{status: probeFailed, err: c.err}
			d.key.vid = c.vid
			if got := d.scanFailure(); !strings.Contains(got, c.want) {
				t.Errorf("scanFailure() = %q, want substring %q", got, c.want)
			}
		})
	}
}

func TestClassifyProbeErr(t *testing.T) {
	if got := classifyProbeErr(fs.ErrPermission); got != errPermission {
		t.Errorf("classifyProbeErr(fs.ErrPermission) = %v, want errPermission", got)
	}
	if got := classifyProbeErr(context.DeadlineExceeded); got != errNoAnswer {
		t.Errorf("classifyProbeErr(DeadlineExceeded) = %v, want errNoAnswer", got)
	}
	// The context cause joined onto a probe error still classifies.
	joined := errors.Join(errors.New("io"), context.DeadlineExceeded)
	if got := classifyProbeErr(joined); got != errNoAnswer {
		t.Errorf("classifyProbeErr(%v) = %v, want errNoAnswer", joined, got)
	}
	other := errors.New("boom")
	if got := classifyProbeErr(other); got != other {
		t.Errorf("classifyProbeErr(boom) = %v, want it unchanged", got)
	}
}

// TestDimComments pins the transcript styling: only #-comment lines are
// dimmed, each closes its own reset, and colour off is the identity.
func TestDimComments(t *testing.T) {
	in := "# a comment\n\nsoft-kvm connect --trigger 046d:c548\n#\n"
	if got := dimComments(in, false); got != in {
		t.Errorf("colour off: got %q, want the input unchanged", got)
	}
	want := "\x1b[2m# a comment\x1b[0m\n\nsoft-kvm connect --trigger 046d:c548\n\x1b[2m#\x1b[0m\n"
	if got := dimComments(in, true); got != want {
		t.Errorf("colour on: got %q, want %q", got, want)
	}
}

// TestDimNonTerminal pins that a writer that is not a terminal — every
// test's bytes.Buffer, and `soft-kvm detect > file` — renders plain.
func TestDimNonTerminal(t *testing.T) {
	if got := Dim(&bytes.Buffer{}, "# a comment\n"); got != "# a comment\n" {
		t.Errorf("a bytes.Buffer must not be styled, got %q", got)
	}
}

// A receiver-paired peripheral is addressed by its receiver's pairing slot,
// never by its own VID:PID: its HID node lives as long as the pairing, so it
// never attaches. Only the slot holding the link is offered.
func TestRenderSuggestionsReceiverSlots(t *testing.T) {
	shadowKbd := probedDevice{hidDevice: hidDevice{
		key:        deviceKey{vid: 0x046d, pid: 0x4088},
		mfr:        "Logitech",
		product:    "ERGO K860",
		interfaces: []ifaceUsage{{0x01, 0x06}},
		shadow:     true,
	}}
	receiver := probedDevice{
		hidDevice: hidDevice{
			key:        deviceKey{vid: 0x046d, pid: 0xc52b},
			mfr:        "Logitech",
			product:    "USB Receiver",
			interfaces: []ifaceUsage{{0xFF00, 0x01}},
		},
		status: probeOK,
		inv: &hidpp.Inventory{
			Kind: hidpp.KindReceiver,
			Paired: []hidpp.PairedDevice{
				{Index: 1, Kind: hidpp.KindMouse},                  // paired, on another host
				{Index: 2, Kind: hidpp.KindKeyboard, Online: true}, // holds the link
			},
		},
	}
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, noScreenWiring(shadowKbd, receiver)); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "--trigger keyboard \\\n  --declare-hid 'keyboard 046d:c52b:2'\n") {
		t.Errorf("missing linked keyboard slot trigger:\n%s", out)
	}
	if strings.Contains(out, "046d:4088") {
		t.Errorf("receiver-shadowed node offered as a trigger:\n%s", out)
	}
	if strings.Contains(out, "046d:c52b:1") {
		t.Errorf("unlinked slot offered as a trigger:\n%s", out)
	}
}

func TestIsTriggerExcludesShadow(t *testing.T) {
	kbd := hidDevice{
		key:        deviceKey{vid: 0x046d, pid: 0x4088},
		interfaces: []ifaceUsage{{0x01, 0x06}},
	}
	if !(probedDevice{hidDevice: kbd}).isTrigger() {
		t.Error("a real keyboard node must be a trigger candidate")
	}
	shadowed := kbd
	shadowed.shadow = true
	if (probedDevice{hidDevice: shadowed}).isTrigger() {
		t.Error("a receiver-shadowed node must not be a trigger candidate")
	}
}

func TestBuildWiringScreens(t *testing.T) {
	dell := screen.Display{Mfg: "DEL", Model: "DELL U2720Q"}
	asus := screen.Display{Mfg: "ACI", Model: "PA278QV", Input: 0x11, InputOK: true}
	lg := screen.Display{Mfg: "GSM", Model: "LG HDR WQHD", Input: 0x0F, InputOK: true}
	laptop := screen.Display{Mfg: "BOE", Model: "Panel"}
	cases := []struct {
		name   string
		report screen.Report
		want   []screenDecl
	}{
		{"backend down names nothing", screen.Report{Err: errors.New("no i2c")},
			[]screenDecl{{"NAME", inputPlaceholder, ""}}},
		{"no monitor", screen.Report{},
			[]screenDecl{{"NAME", inputPlaceholder, ""}}},
		{"the port is unknown while another host is on screen", screen.Report{Displays: []screen.Display{dell}},
			[]screenDecl{{"DELL U2720Q", inputPlaceholder, ""}}},
		{"every monitor that answers is declared", screen.Report{Displays: []screen.Display{dell, asus}},
			[]screenDecl{{"DELL U2720Q", inputPlaceholder, ""}, {"PA278QV", "hdmi1", ""}}},
		{"a vendor table is spelled out", screen.Report{Displays: []screen.Display{lg}},
			[]screenDecl{{"LG HDR WQHD", "dp1", "lg"}}},
		{"a port that maps to no alias is a placeholder", screen.Report{Displays: []screen.Display{{Mfg: "DEL", Model: "D", Input: 0x42, InputOK: true}}},
			[]screenDecl{{"D", inputPlaceholder, ""}}},
		{"an unreachable monitor still names its table, ahead of a panel", screen.Report{Err: errors.New("denied"), Unreachable: []screen.Display{laptop, {Mfg: "GSM", Model: "LG HDR WQHD"}}},
			[]screenDecl{{"LG HDR WQHD", inputPlaceholder, "lg"}}},
		{"an unreachable monitor alone", screen.Report{Err: errors.New("denied"), Unreachable: []screen.Display{dell}},
			[]screenDecl{{"DELL U2720Q", inputPlaceholder, ""}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildWiring(nil, c.report, nil).screens; !slices.Equal(got, c.want) {
				t.Errorf("screens = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDeclFlagQuoting(t *testing.T) {
	// A quote in a name survives the shell's single quotes.
	got := declFlag("declare-screen", screenDecl{name: "it's", input: "dp1"}.words())
	if want := `--declare-screen '"it'\''s" dp1'`; got != want {
		t.Errorf("declFlag = %q, want %q", got, want)
	}
}

func TestRenderNotes(t *testing.T) {
	t.Run("backend down says why", func(t *testing.T) {
		var buf bytes.Buffer
		r := screen.Report{Backend: "ddcutil", Err: errors.New("ddcutil: exit status 1: No /dev/i2c devices exist.\nddcutil requires module i2c-dev.")}
		if err := renderNotes(&buf, r, buildWiring(nil, r, nil)); err != nil {
			t.Fatalf("renderNotes: %v", err)
		}
		out := buf.String()
		want := "# ⚠️ ddcutil: exit status 1: No /dev/i2c devices exist.\n" +
			"#    ddcutil requires module i2c-dev.\n"
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
		if !strings.Contains(out, "Replace DP1_HDMI_ETC") {
			t.Errorf("a placeholder with no explanation:\n%s", out)
		}
	})

	t.Run("a known port needs no placeholder note", func(t *testing.T) {
		var buf bytes.Buffer
		r := screen.Report{Backend: "ddcutil", Displays: []screen.Display{
			{Mfg: "DEL", Model: "DELL U2720Q", Input: 0x0F, InputOK: true},
		}}
		if err := renderNotes(&buf, r, buildWiring(nil, r, nil)); err != nil {
			t.Fatalf("renderNotes: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "# Monitor: DELL U2720Q (DEL), mapping table standard, showing this host on dp1.\n") {
			t.Errorf("missing the monitor line:\n%s", out)
		}
		if strings.Contains(out, "Replace") {
			t.Errorf("a note about a placeholder that is not there:\n%s", out)
		}
	})
}

// A linked Easy-Switch trigger whose channel could not be read is declared
// without host=N, so the command still runs, and the notes say what is lost.
func TestUnreadChannelOfLinkedTrigger(t *testing.T) {
	receiver := probedReceiver(0xc548,
		hidpp.PairedDevice{Index: 1, Kind: hidpp.KindKeyboard, Name: "MX Keys", Online: true},
		hidpp.PairedDevice{Index: 2, Kind: hidpp.KindMouse, Name: "LIFT", Online: true})
	r := screen.Report{}
	// The mouse answered its channel, the keyboard did not.
	w := buildWiring([]probedDevice{receiver}, r, map[hidpp.Target]uint8{{VID: 0x046d, PID: 0xc548, Slot: 2}: 1})
	var buf bytes.Buffer
	if err := renderSuggestions(&buf, w); err != nil {
		t.Fatalf("renderSuggestions: %v", err)
	}
	if err := renderNotes(&buf, r, w); err != nil {
		t.Fatalf("renderNotes: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"soft-kvm connect --trigger mx-keys \\\n" +
			"  --declare-hid 'mx-keys 046d:c548:1' \\\n" +
			"  --declare-hid 'lift 046d:c548:2 host=1' \\\n" +
			"  -A take-hid\n",
		// Following the mouse still declares the keyboard: this host gives it
		// away when another takes it.
		"soft-kvm connect --trigger lift \\\n" +
			"  --declare-hid 'mx-keys 046d:c548:1' \\\n" +
			"  --declare-hid 'lift 046d:c548:2 host=1' \\\n" +
			"  -A take-hid\n",
		"# ⚠️ detect could not read the Easy-Switch channel of mx-keys: add host=N\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Add flags for the following devices") {
		t.Errorf("a device the command declares is listed as left out:\n%s", out)
	}
	assertTranscript(t, out)
}
