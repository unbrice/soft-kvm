// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// hidpp.go: Logitech HID++ changeHost (0x1814) host switching — the
// `hid-switch` virtual switch command (SPEC §5.5) — plus the HID++ 1.0
// register reads used to inventory receiver pairing tables. Pure report
// framing is split from I/O so it can be table-tested without hardware.

package hidpp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/telesma-app/hid"
)

const (
	reportShort = 0x10 // 7-byte HID++ report — all requests are sent short
	reportLong  = 0x11 // 20-byte HID++ report — a reply size only

	// Error replies arrive in a normal short/long report; the sub-ID byte
	// (report[2]) carries the error indicator, followed by the request's
	// feature index, function|swID and the error code.
	errSubShort = 0x8F // error reply in a short report
	errSubLong  = 0xFF // error reply in a long report

	// HID++ 1.0 register reads, answered locally by receivers at 0xFF.
	subRegReadShort = 0x81 // short register read
	subRegReadLong  = 0x83 // long register read (registers 0x200+)

	regReceiverInfo = 0x2B5 // receiver info register: the pairing table

	// Pairing-info sub-registers per slot: Unifying 0x20+slot-1, Bolt
	// 0x50+slot (Solaar hidpp10_constants.py InfoSubRegisters).
	subPairingUnifying = 0x20
	subPairingBolt     = 0x50

	featRoot       = 0x0000 // IRoot: fn 0 getFeature, always at index 0
	featFeatureSet = 0x0001 // IFeatureSet: every HID++ 2.0 device has it
	featDeviceName = 0x0005 // GetDeviceNameType: fn 2 is getDeviceType
	featChangeHost = 0x1814 // changeHost: fn 1 is setHost

	fnGetFeature    = 0
	fnGetHostInfo   = 0
	fnSetHost       = 1
	fnGetDeviceType = 2

	swID = 0x1 // software id nibble, matched against replies

	// subDeviceConnection is the HID++ 1.0 device-connection notification a
	// receiver sends, unprompted, whenever a paired device's radio link comes
	// up or goes down. Bit 0x40 of its first parameter is set when the link is
	// NOT established (Solaar notifications.py).
	subDeviceConnection = 0x41
	linkNotEstablished  = 0x40

	// DeviceIndexDirect addresses the device the HID interface belongs to
	// (Bluetooth pairing or own dongle). Devices behind a Bolt/Unifying
	// receiver are addressed by pairing slot 1-6 through the receiver's
	// interface.
	DeviceIndexDirect = 0xFF

	probeTimeout   = 500 * time.Millisecond // per request while probing
	setHostTimeout = 1 * time.Second        // waiting for the setHost ACK

	// linkReadTimeout bounds one WatchLinks read so cancellation stays
	// responsive. Expiry is the normal case: links change rarely.
	linkReadTimeout = 500 * time.Millisecond

	probeAttempts = 3 // handshake nudges for a dozing Bluetooth device
)

// Kind is the HID++ device type byte returned by getDeviceType.
type Kind uint8

const (
	KindKeyboard Kind = 0x00
	KindMouse    Kind = 0x03
	KindReceiver Kind = 0x07
	KindUnknown  Kind = 0xFF
)

func (k Kind) String() string {
	switch k {
	case KindKeyboard:
		return "keyboard"
	case KindMouse:
		return "mouse"
	case KindReceiver:
		return "receiver"
	default:
		return fmt.Sprintf("kind 0x%02x", uint8(k))
	}
}

// MaxPairingSlot is the highest addressable receiver pairing slot.
const MaxPairingSlot = 6

// MaxChannel is the highest Easy-Switch channel, as printed on the key.
const MaxChannel = 3

// ParseTarget parses the one device address this tool uses everywhere:
// "VID:PID" or "VID:PID:SLOT", hex VID and PID, decimal 1-based pairing slot.
// slot is 0 when none was given. --trigger and hid-switch share it so the two
// can never drift into different spellings of the same device.
func ParseTarget(s string) (vid, pid uint16, slot uint8, err error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid device %q: want VID:PID or VID:PID:SLOT", s)
	}
	v, err := strconv.ParseUint(parts[0], 16, 16)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid VID %q: %w", parts[0], err)
	}
	p, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid PID %q: %w", parts[1], err)
	}
	if len(parts) == 3 {
		n, err := strconv.ParseUint(parts[2], 10, 8)
		if err != nil || n < 1 || n > MaxPairingSlot {
			return 0, 0, 0, fmt.Errorf("invalid pairing slot %q in %q: want 1-%d", parts[2], s, MaxPairingSlot)
		}
		slot = uint8(n)
	}
	return uint16(v), uint16(p), slot, nil
}

// Switch is a parsed `hid-switch VID:PID[:SLOT] [host=N]` command (SPEC §5.5).
type Switch struct {
	VID, PID uint16
	// Slot 0 means "every device linked to this receiver right now" — on the
	// losing host that is exactly the set that should follow the one that
	// already left. A direct device is addressed as slot 0 too.
	Slot uint8
	// Channel is an explicit Easy-Switch channel (1-MaxChannel); 0 means take
	// the winner's channel from the server state at run time.
	Channel uint8
}

// Parse validates a hid-switch argv (without the "hid-switch" word itself).
//
// The target host is normally omitted: the winning host publishes its own
// Easy-Switch channel with its claim, and the loser switches the peripherals
// to that. It cannot be worked out locally — a device reports how many hosts
// it supports and which one it is on, but not which of the others is the peer
// (SPEC §5.5). "host=N" overrides it, and N is the channel as printed on the
// key (1-MaxChannel), matching the 1-based pairing slots.
func Parse(args []string) (*Switch, error) {
	if len(args) != 1 && len(args) != 2 {
		return nil, errors.New("usage: hid-switch VID:PID[:SLOT] [host=N]")
	}
	vid, pid, slot, err := ParseTarget(args[0])
	if err != nil {
		return nil, err
	}
	s := &Switch{VID: vid, PID: pid, Slot: slot}
	if len(args) == 2 {
		spec, ok := strings.CutPrefix(args[1], "host=")
		if !ok {
			return nil, fmt.Errorf("invalid argument %q: want host=N (channel 1-%d)", args[1], MaxChannel)
		}
		n, err := strconv.ParseUint(spec, 10, 8)
		if err != nil || n < 1 || n > MaxChannel {
			return nil, fmt.Errorf("invalid host channel %q: want 1-%d, as printed on the key", spec, MaxChannel)
		}
		s.Channel = uint8(n)
	}
	return s, nil
}

// Do switches the selected devices to a host channel. fallbackChannel is used
// when the command carried no explicit host=N; it is the winner's channel,
// taken from the server state. Both being zero is a configuration error, not a
// silent no-op — switching to a guessed host moves a peripheral somewhere the
// user cannot reach it.
func (s *Switch) Do(ctx context.Context, fallbackChannel uint8) error {
	channel := s.Channel
	if channel == 0 {
		channel = fallbackChannel
	}
	if channel < 1 || channel > MaxChannel {
		return fmt.Errorf("hid-switch %04x:%04x: no target host channel: the owner "+
			"published none and the command has no host=N (SPEC §5.5)", s.VID, s.PID)
	}
	// The wire and the key legend are off by one: changeHost takes 0-2.
	hostIndex := channel - 1

	c, err := openInterface(ctx, s.VID, s.PID)
	if err != nil {
		// A named device that is simply not here has nothing to move, and
		// the losing host runs this on every switch: failing would trip the
		// §4.3 breaker for a peripheral the user already moved by hand.
		if s.Slot == 0 && errors.Is(err, ErrNoInterface) {
			slog.Info("hid-switch: device not on this host, nothing to move",
				"device", fmt.Sprintf("%04x:%04x", s.VID, s.PID))
			return nil
		}
		return err
	}
	defer func() {
		if err := c.dev.Close(); err != nil {
			slog.Warn("hid-switch: device close failed", "error", err)
		}
	}()

	var devIdxs []uint8
	switch {
	case s.Slot != 0:
		devIdxs = []uint8{s.Slot}
	default:
		devIdxs, err = c.resolveLinked(ctx)
		if err != nil {
			return err
		}
		if len(devIdxs) == 0 {
			// Nothing is linked here, so nothing needs moving. That is the
			// normal outcome when the user switched every peripheral by hand.
			slog.Info("hid-switch: nothing linked here, nothing to move",
				"device", fmt.Sprintf("%04x:%04x", s.VID, s.PID))
			return nil
		}
	}

	var errs []error
	for _, devIdx := range devIdxs {
		feat, err := c.getFeatureNudged(ctx, devIdx, featChangeHost)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if feat == 0 {
			errs = append(errs, fmt.Errorf("device at index %d does not support changeHost (0x1814)", devIdx))
			continue
		}
		slog.Info("hid-switch: switching host",
			"device", fmt.Sprintf("%04x:%04x", s.VID, s.PID),
			"deviceIndex", devIdx, "channel", channel)
		errs = append(errs, c.setHost(ctx, feat, devIdx, hostIndex))
	}
	return errors.Join(errs...)
}

// resolveLinked returns the device indexes to move when no slot was given:
// the directly attached device when this interface is one, else every pairing
// slot of the receiver whose device holds the link right now.
//
// On the losing host that set is exactly right: the peripheral that carried
// the gesture has already left on its own, so what remains linked is what has
// to follow it. It makes the command symmetric — the same argv serves
// follow-the-keyboard and follow-the-mouse.
func (c *conn) resolveLinked(ctx context.Context) ([]uint8, error) {
	kctx, cancel := context.WithTimeout(ctx, probeTimeout)
	kind, err := c.getKind(kctx, DeviceIndexDirect)
	cancel()
	if err == nil && kind != KindReceiver {
		return []uint8{DeviceIndexDirect}, nil
	}
	paired, ok := c.pairingTable(ctx)
	if !ok {
		if err != nil {
			return nil, fmt.Errorf("neither a directly attached device nor a receiver: %w", err)
		}
		return nil, errors.New("no pairing table and no directly attached device")
	}
	var out []uint8
	for _, p := range paired {
		if p.Online {
			out = append(out, p.Index)
		}
	}
	return out, nil
}

// CurrentChannel reports which Easy-Switch channel (1-MaxChannel) this host
// occupies on the given device — changeHost's getHostInfo, which returns the
// host count and the current host index. slot 0 addresses a directly attached
// device.
//
// This is the winner's half of the host-channel problem: a host can always
// learn its *own* channel, never the peer's, so the winner publishes it.
func CurrentChannel(ctx context.Context, vid, pid uint16, slot uint8) (uint8, error) {
	c, err := openInterface(ctx, vid, pid)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.dev.Close() }()

	devIdx := uint8(DeviceIndexDirect)
	if slot != 0 {
		devIdx = slot
	}
	feat, err := c.getFeatureNudged(ctx, devIdx, featChangeHost)
	if err != nil {
		return 0, err
	}
	if feat == 0 {
		return 0, fmt.Errorf("device %04x:%04x index %d does not support changeHost (0x1814)", vid, pid, devIdx)
	}
	reply, err := c.request(ctx, devIdx, feat, fnGetHostInfo)
	if err != nil {
		return 0, err
	}
	if len(reply) < 2 {
		return 0, errors.New("short getHostInfo reply")
	}
	// reply[0] is the host count, reply[1] the current host index (0-based).
	if reply[1] >= reply[0] {
		return 0, fmt.Errorf("getHostInfo: current host %d outside the %d it reports", reply[1], reply[0])
	}
	return reply[1] + 1, nil
}

// Error is a HID++ error reply (sub-ID 0x8F/0xFF) from the device.
type Error struct{ Code uint8 }

func (e *Error) Error() string { return fmt.Sprintf("hid++ error 0x%02x", e.Code) }

// deviceConn is the subset of *hid.Device used by conn, extracted so tests
// can fake I/O without touching hardware.
type deviceConn interface {
	Write(ctx context.Context, p []byte) (int, error)
	Read(ctx context.Context, p []byte) (int, error)
	Close() error
}

// conn is an open HID++ interface.
type conn struct {
	dev deviceConn
}

// openInterface returns an HID++-answering interface of VID:PID. Vendor-defined
// interfaces are tried first because Logitech receivers expose HID++ on usage
// pages ≥0xFF00; Bluetooth devices may flatten the HID++ collection into the
// same node as the mouse/keyboard collection, so ordinary interfaces are tried
// next. Each candidate must still pass speaksHIDPP.
func openInterface(ctx context.Context, vid, pid uint16) (*conn, error) {
	var vendor, other []*hid.DeviceInfo
	for info, err := range hid.Enumerate(hid.WithVendorID(vid), hid.WithProductID(pid)) {
		if err != nil {
			return nil, err
		}
		if info.UsagePage >= 0xFF00 {
			vendor = append(vendor, info)
		} else {
			other = append(other, info)
		}
	}
	if len(vendor)+len(other) == 0 {
		return nil, fmt.Errorf("%w for %04x:%04x", ErrNoInterface, vid, pid)
	}

	var tried int
	var openErrs []error
	var lastErr, rejectedErr error
	for _, info := range append(vendor, other...) {
		tried++
		slog.Debug("hidpp: trying interface", "path", info.Path,
			"usagePage", fmt.Sprintf("%04x", info.UsagePage),
			"usage", fmt.Sprintf("%04x", info.Usage))
		dev, err := hid.OpenPath(info.Path)
		if err != nil {
			slog.Debug("hid-switch: cannot open interface", "path", info.Path, "error", err)
			openErrs = append(openErrs, err)
			continue
		}
		c := &conn{dev: dev}
		if err := c.speaksHIDPP(ctx); err != nil {
			// A rejected report means the interface cannot carry HID++ at
			// all: keep it as a fallback cause, but a timeout ("no answer")
			// describes the failure better.
			if isReportRejected(err) {
				rejectedErr = err
			} else {
				lastErr = err
			}
			_ = dev.Close()
			continue
		}
		return c, nil
	}
	if len(openErrs) == tried {
		// None of the candidates could even be opened — on Linux that is
		// usually the missing hidraw write permission (SPEC §5.5).
		return nil, fmt.Errorf("cannot open a HID interface for %04x:%04x: %w", vid, pid, errors.Join(openErrs...))
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no HID++ interface answered for %04x:%04x (%d tried): %w", vid, pid, tried, lastErr)
	}
	if rejectedErr != nil {
		return nil, fmt.Errorf("no interface for %04x:%04x carries the HID++ report (%d tried): %w", vid, pid, tried, rejectedErr)
	}
	return nil, fmt.Errorf("no HID++ interface answered for %04x:%04x (%d tried)", vid, pid, tried)
}

// isReportRejected reports whether the interface rejected the HID++ report on
// write: the report ID is not in the interface's report descriptor (EPIPE or
// EINVAL from a hidraw write), so the interface cannot speak HID++ at all.
func isReportRejected(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EINVAL)
}

// speaksHIDPP reports whether the interface answers Logitech-framed requests.
// Any framed reply counts — even an error reply or "feature unsupported". The
// handshake is repeated up to probeAttempts times on silence: a dozing
// Bluetooth device may ignore the first nudges before its vendor channel
// wakes. probeTimeout bounds one nudge.
//
// When the direct index stays silent to HID++ 2.0, probe the receiver
// pairing-table register instead: receivers speak only HID++ 1.0 registers at
// 0xFF, answered locally from their own flash — no RF, sub-millisecond — and
// even an error reply (wrong layout, empty slot) proves Logitech framing.
func (c *conn) speaksHIDPP(ctx context.Context) error {
	var err error
	for i := 0; i < probeAttempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, err = c.getFeature(rctx, DeviceIndexDirect, featFeatureSet)
		cancel()
		var herr *Error
		if err == nil || errors.As(err, &herr) {
			return nil
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return err // write errors and the like are not sleep — don't retry
		}
		slog.Debug("hidpp: handshake unanswered, nudging again", "attempt", i+1)
	}
	rctx, cancel := context.WithTimeout(ctx, probeTimeout)
	_, rerr := c.readRegister(rctx, DeviceIndexDirect, regReceiverInfo, subPairingUnifying)
	cancel()
	var herr *Error
	if rerr == nil || errors.As(rerr, &herr) {
		slog.Debug("hidpp: interface answered a HID++ 1.0 register read — a receiver")
		return nil
	}
	if !errors.Is(rerr, context.DeadlineExceeded) {
		return rerr
	}
	return err
}

// getFeature maps a feature ID to its index on the device at devIdx via
// IRoot. Index 0 means the device does not support the feature. The caller's
// ctx deadline bounds the exchange.
func (c *conn) getFeature(ctx context.Context, devIdx uint8, feature uint16) (uint8, error) {
	reply, err := c.request(ctx, devIdx, 0, fnGetFeature, byte(feature>>8), byte(feature))
	if err != nil {
		return 0, err
	}
	if len(reply) == 0 {
		return 0, errors.New("empty getFeature reply")
	}
	return reply[0], nil
}

// getFeatureNudged retries getFeature on silence: a dozing wireless device
// wakes on the first request (RF cold wake is ~700ms, past one probeTimeout)
// and answers a later one. Only deadline misses retry — an error reply or a
// write failure is an answer, not sleep.
func (c *conn) getFeatureNudged(ctx context.Context, devIdx uint8, feature uint16) (uint8, error) {
	var err error
	for i := 0; i < probeAttempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, probeTimeout)
		var feat uint8
		feat, err = c.getFeature(rctx, devIdx, feature)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			return feat, err
		}
		slog.Debug("hidpp: no answer, nudging again", "deviceIndex", devIdx, "attempt", i+1)
	}
	return 0, err
}

// getKind reads the device type byte (feature 0x0005, fn 2).
func (c *conn) getKind(ctx context.Context, devIdx uint8) (Kind, error) {
	idx, err := c.getFeature(ctx, devIdx, featDeviceName)
	if err != nil {
		return KindUnknown, err
	}
	if idx == 0 {
		return KindUnknown, nil
	}
	reply, err := c.request(ctx, devIdx, idx, fnGetDeviceType)
	if err != nil {
		return KindUnknown, err
	}
	if len(reply) == 0 {
		return KindUnknown, errors.New("empty getDeviceType reply")
	}
	return Kind(reply[0]), nil
}

// resolveKind resolves a kind target to device indexes: the directly attached
// device when it matches the kind, else every pairing slot that does. The
// direct check comes first because a Bluetooth device answers pairing-slot
// scans as itself, once per slot — scanning first would switch it several
// times. Each exchange gets probeTimeout, not the ctx deadline: one call is
// several exchanges.
func (c *conn) resolveKind(ctx context.Context, kind Kind) ([]uint8, error) {
	kctx, cancel := context.WithTimeout(ctx, probeTimeout)
	k, err := c.getKind(kctx, DeviceIndexDirect)
	cancel()
	if err == nil && k == kind {
		return []uint8{DeviceIndexDirect}, nil
	}
	return c.findSlots(ctx, kind)
}

// findSlots finds every paired device of the given kind that supports
// changeHost. The occupied slots and their kinds come from the receiver's
// local pairing table — no RF — so only matching slots get a (nudged)
// wireless changeHost query. resolveKind calls this only when the directly
// attached device does not match.
func (c *conn) findSlots(ctx context.Context, kind Kind) ([]uint8, error) {
	paired, ok := c.pairingTable(ctx)
	if !ok {
		return nil, fmt.Errorf("no receiver pairing table for a kind target")
	}
	var slots []uint8
	for _, p := range paired {
		if p.Kind != kind {
			continue
		}
		feat, err := c.getFeatureNudged(ctx, p.Index, featChangeHost)
		if err == nil && feat != 0 {
			slots = append(slots, p.Index)
		}
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("no paired %s supports changeHost", kind)
	}
	return slots, nil
}

// errLinkDropped marks a read that failed because the device went away
// mid-exchange (EIO on Linux hidraw). For setHost that is the device
// switching hosts mid-ACK — a success, not a failure (SPEC §5.5).
var errLinkDropped = errors.New("hidpp: link dropped")

// ErrNoInterface means the device is not present on this host at all. For the
// automatic hid-switch form that is not a failure: a peripheral that already
// left has nothing left to move.
var ErrNoInterface = errors.New("no HID interface")

// setHost sends changeHost's fn 1, waiting up to setHostTimeout for the ACK.
// A matching ACK is success; an error reply is a failure; no reply at all is
// also success, because a device that switched drops the link to this host
// mid-reply — over Bluetooth that drop surfaces as a read error, not a
// timeout.
func (c *conn) setHost(ctx context.Context, featIdx, devIdx, host uint8) error {
	rctx, cancel := context.WithTimeout(ctx, setHostTimeout)
	defer cancel()
	_, err := c.request(rctx, devIdx, featIdx, fnSetHost, host)
	if ctx.Err() == nil &&
		(errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errLinkDropped)) {
		slog.Debug("hid-switch: no ACK, the device presumably switched away")
		return nil
	}
	return err
}

// request sends one HID++ 2.0 request and waits for its reply. An error
// reply comes back as *Error. The caller's ctx deadline bounds the exchange.
func (c *conn) request(ctx context.Context, devIdx, featIdx, fn uint8, params ...byte) ([]byte, error) {
	return c.exchange(ctx, buildReport(devIdx, featIdx, fn, params),
		func(r []byte) ([]byte, error) { return matchReply(r, devIdx, featIdx, fn) })
}

// exchange writes one report, then reads until match accepts a reply,
// discarding unrelated reports (notifications). Replies may arrive in either
// report size. A link drop mid-exchange (EIO on Linux hidraw) comes back
// wrapped in errLinkDropped. The caller's ctx deadline bounds the exchange.
func (c *conn) exchange(ctx context.Context, report []byte, match func([]byte) ([]byte, error)) ([]byte, error) {
	slog.Debug("hidpp: report out", "report", fmt.Sprintf("%x", report))
	if _, err := c.dev.Write(ctx, report); err != nil {
		slog.Debug("hidpp: write failed", "error", err)
		return nil, err
	}
	buf := make([]byte, 64)
	for {
		n, err := c.dev.Read(ctx, buf)
		if err != nil {
			switch {
			// A device gone mid-exchange (EIO on Linux hidraw).
			case errors.Is(err, syscall.EIO) && ctx.Err() == nil:
				slog.Debug("hidpp: link dropped mid-exchange")
				return nil, fmt.Errorf("%w: %w", errLinkDropped, err)
			case errors.Is(err, context.DeadlineExceeded):
				slog.Debug("hidpp: no reply (silence, or a device ignoring the request)")
			default:
				slog.Debug("hidpp: read failed", "error", err)
			}
			return nil, err
		}
		slog.Debug("hidpp: report in", "report", fmt.Sprintf("%x", buf[:n]))
		if reply, herr := match(buf[:n]); reply != nil || herr != nil {
			return reply, herr
		}
	}
}

// buildReport frames a HID++ 2.0 request in the short report: report ID,
// device index, feature index, function<<4|swID, then the parameters. Every
// HID++ device carries the 7-byte short report and our requests never exceed
// its 3 parameter bytes; the 20-byte long report is a reply size here —
// Logitech receivers STALL the 20-byte SET_REPORT on their control pipe.
func buildReport(devIdx, featIdx, fn uint8, params []byte) []byte {
	r := make([]byte, 7)
	r[0] = reportShort
	r[1] = devIdx
	r[2] = featIdx
	r[3] = fn<<4 | swID
	copy(r[4:], params)
	return r
}

// matchReply picks the reply to a request out of the input stream. It
// returns (params, nil) for a matching data reply, (nil, *Error) for a
// matching error reply, and (nil, nil) for anything else.
//
// Data reply:  reportID, devIdx, featIdx, fn|swID, params...
// Error reply: reportID, devIdx, errSubShort|errSubLong, featIdx, fn|swID, code
func matchReply(report []byte, devIdx, featIdx, fn uint8) ([]byte, error) {
	if len(report) < 5 || report[1] != devIdx {
		return nil, nil
	}
	switch report[0] {
	case reportShort, reportLong:
	default:
		return nil, nil
	}
	fnSw := fn<<4 | swID
	if report[2] == featIdx && report[3] == fnSw {
		return report[4:], nil
	}
	if (report[2] == errSubShort || report[2] == errSubLong) && len(report) >= 6 &&
		report[3] == featIdx && report[4] == fnSw {
		return nil, &Error{Code: report[5]}
	}
	return nil, nil
}

// readRegister performs a HID++ 1.0 register read — answered locally by
// receivers at 0xFF, with no RF traffic. Registers below 0x200 are read with
// sub-command 0x81, long ones with 0x83; long-register replies arrive in a
// long report. The reply echoes the sub-register before the register data;
// the return starts at that echo (Solaar's slicing convention). A 1.0 error
// reply comes back as *Error.
func (c *conn) readRegister(ctx context.Context, devIdx uint8, register uint16, subreg byte) ([]byte, error) {
	sub := byte(subRegReadShort)
	if register >= 0x200 {
		sub = subRegReadLong
	}
	addr := byte(register)
	report := []byte{reportShort, devIdx, sub, addr, subreg, 0, 0}
	return c.exchange(ctx, report, func(r []byte) ([]byte, error) {
		return matchRegisterReply(r, devIdx, sub, addr, subreg)
	})
}

// matchRegisterReply picks the reply to a HID++ 1.0 register read out of the
// input stream, same return contract as matchReply.
//
// Data reply:  reportID, devIdx, sub, addr, subreg, data...
// Error reply: reportID, devIdx, 0x8F, sub, addr, code
func matchRegisterReply(report []byte, devIdx, sub, addr, subreg byte) ([]byte, error) {
	if len(report) < 5 || report[1] != devIdx {
		return nil, nil
	}
	switch report[0] {
	case reportShort, reportLong:
	default:
		return nil, nil
	}
	if report[2] == errSubShort && len(report) >= 6 && report[3] == sub && report[4] == addr {
		return nil, &Error{Code: report[5]}
	}
	if report[2] == sub && report[3] == addr && report[4] == subreg {
		return report[4:], nil
	}
	return nil, nil
}

// PairedDevice is one occupied receiver slot.
type PairedDevice struct {
	Index  uint8
	Kind   Kind
	Online bool // the device is linked to this receiver right now
}

// Inventory is what Probe learned about one VID:PID: the kind of the device
// itself and, for receivers, the occupied pairing slots.
type Inventory struct {
	Kind   Kind
	Paired []PairedDevice
}

// Probe opens the first HID++ interface of VID:PID and reports the device
// kind; for a receiver it also reports the occupied pairing slots. Used by
// `detect` to draw the slot map (SPEC §5.5).
func Probe(ctx context.Context, vid, pid uint16) (*Inventory, error) {
	c, err := openInterface(ctx, vid, pid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.dev.Close() }()

	kctx, cancel := context.WithTimeout(ctx, probeTimeout)
	kind, kerr := c.getKind(kctx, DeviceIndexDirect)
	cancel()
	if kerr == nil && kind != KindReceiver {
		// A directly attached device: don't scan slots — a Bluetooth device
		// answers pairing-slot scans as itself, once per slot.
		return &Inventory{Kind: kind}, nil
	}
	// A receiver speaks only HID++ 1.0 registers at 0xFF. Its pairing table
	// is local flash: no RF, and no dependence on the paired devices being
	// awake, on, or switched to this host.
	if paired, ok := c.pairingTable(ctx); ok {
		return &Inventory{Kind: KindReceiver, Paired: paired}, nil
	}
	if kerr != nil {
		return nil, fmt.Errorf("answered the handshake but reads fail now: %w", kerr)
	}
	return &Inventory{Kind: kind}, nil
}

// pairingTable reads the receiver's local pairing table (register 0x2B5) and
// returns the occupied slots. ok is false when nothing answers the register
// reads at all — the interface is not a receiver.
func (c *conn) pairingTable(ctx context.Context) (paired []PairedDevice, ok bool) {
	for slot := uint8(1); slot <= 6; slot++ {
		data, bolt, answered := c.readPairingInfo(ctx, slot)
		if !answered {
			// Silence on the very first read means no receiver; a receiver
			// that answered before does not go silent mid-table.
			if slot == 1 {
				return nil, false
			}
			continue
		}
		ok = true
		if data != nil {
			paired = append(paired, PairedDevice{
				Index:  slot,
				Kind:   pairingKind(bolt, data),
				Online: c.slotOnline(ctx, slot),
			})
		}
	}
	return paired, ok
}

// readPairingInfo reads one slot's pairing info, trying the Unifying layout
// (sub-register 0x20+slot-1) then the Bolt one (0x50+slot). answered is false
// on silence; data is nil when the slot is empty (error reply on both
// layouts) — an empty slot and a wrong layout both draw an immediate error,
// so both layouts are always tried before a slot is declared empty.
func (c *conn) readPairingInfo(ctx context.Context, slot uint8) (data []byte, bolt, answered bool) {
	for i, subreg := range []byte{subPairingUnifying + slot - 1, subPairingBolt + slot} {
		rctx, cancel := context.WithTimeout(ctx, probeTimeout)
		d, err := c.readRegister(rctx, DeviceIndexDirect, regReceiverInfo, subreg)
		cancel()
		if err == nil {
			return d, i == 1, true
		}
		var herr *Error
		if errors.As(err, &herr) {
			answered = true
		}
	}
	return nil, false, answered
}

// pairingKind extracts the device kind from a pairing-info reply. data starts
// at the echoed sub-register byte (Solaar's slicing convention): the HID++
// 1.0 kind nibble is at data[7] on Unifying, data[1] on Bolt.
func pairingKind(bolt bool, data []byte) Kind {
	var nibble byte
	if bolt {
		if len(data) < 2 {
			return KindUnknown
		}
		nibble = data[1] & 0x0F
	} else {
		if len(data) < 8 {
			return KindUnknown
		}
		nibble = data[7] & 0x0F
	}
	switch nibble {
	case 0x01:
		return KindKeyboard
	case 0x02:
		return KindMouse
	default:
		return KindUnknown
	}
}

// slotOnline reports whether the device in a receiver pairing slot is linked
// right now. The probe is one HID++ 2.0 request addressed to the slot: an
// unlinked slot is refused locally by the receiver in about 2ms, while a
// linked device answers over RF — so a scan of six slots costs milliseconds
// per dead slot and one round trip per live one.
//
// Any error is "not linked": an error reply is the receiver refusing to
// route, and silence is a device that would not answer a trigger either.
func (c *conn) slotOnline(ctx context.Context, slot uint8) bool {
	_, err := c.getFeatureNudged(ctx, slot, featRoot)
	return err == nil
}

// SlotOnline reports whether the device paired in slot is linked to the
// receiver at VID:PID right now.
func SlotOnline(ctx context.Context, vid, pid uint16, slot uint8) (bool, error) {
	c, err := openInterface(ctx, vid, pid)
	if err != nil {
		return false, err
	}
	defer func() { _ = c.dev.Close() }()
	return c.slotOnline(ctx, slot), nil
}

// LinkEvent is one receiver device-connection notification: a paired device's
// radio link came up or went down.
type LinkEvent struct {
	Slot        uint8
	Established bool
}

// WatchLinks sends one LinkEvent per device-connection notification from the
// receiver at VID:PID until ctx is done, then returns nil.
//
// This is a passive tap, not a poll. A receiver cannot be asked for live link
// state: register 0x02 returns the paired-device count, and the "announce
// every device" write (0x02 <- 0x02) replays a fabricated arrival whose link
// bit does not track reality. The receiver does volunteer the truth the
// instant a link changes, which is where the kernel's own view comes from
// (SPEC §6.1).
func WatchLinks(ctx context.Context, vid, pid uint16, events chan<- LinkEvent) error {
	c, err := openInterface(ctx, vid, pid)
	if err != nil {
		return err
	}
	defer func() { _ = c.dev.Close() }()

	buf := make([]byte, 64)
	for {
		if ctx.Err() != nil {
			return nil
		}
		// A bounded read keeps ctx cancellation responsive; the deadline
		// expiring just means no notification arrived, which is the norm.
		rctx, cancel := context.WithTimeout(ctx, linkReadTimeout)
		n, rerr := c.dev.Read(rctx, buf)
		cancel()
		if rerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(rerr, context.DeadlineExceeded) {
				continue
			}
			return fmt.Errorf("hidpp: receiver %04x:%04x read: %w", vid, pid, rerr)
		}
		r := buf[:n]
		// The report ID is checked too: r[2] alone is a sub-ID in a 1.0
		// notification but a feature index in a 2.0 reply, and only a
		// HID++-framed report may be read with the 1.0 layout.
		if len(r) < 5 || (r[0] != reportShort && r[0] != reportLong) ||
			r[2] != subDeviceConnection {
			continue
		}
		ev := LinkEvent{Slot: r[1], Established: r[4]&linkNotEstablished == 0}
		slog.Debug("hidpp: link notification",
			"slot", ev.Slot, "established", ev.Established,
			"report", fmt.Sprintf("%x", r))
		select {
		case events <- ev:
		case <-ctx.Done():
			return nil
		}
	}
}
