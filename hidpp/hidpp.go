// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// hidpp.go: Logitech HID++ 2.0 changeHost (0x1814) host switching — the
// `hid-switch` virtual switch command (SPEC §5.5). Pure report framing is
// split from I/O so it can be table-tested without hardware.

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
	reportShort    = 0x10 // 7-byte HID++ report
	reportLong     = 0x11 // 20-byte HID++ report
	reportErrShort = 0x8F // error reply to a short report
	reportErrLong  = 0xFF // error reply to a long report

	featRoot       = 0x0000 // IRoot: fn 0 getFeature, always at index 0
	featFeatureSet = 0x0001 // IFeatureSet: every HID++ 2.0 device has it
	featDeviceName = 0x0005 // GetDeviceNameType: fn 2 is getDeviceType
	featChangeHost = 0x1814 // changeHost: fn 1 is setHost

	fnGetFeature    = 0
	fnSetHost       = 1
	fnGetDeviceType = 2

	swID = 0x1 // software id nibble, matched against replies

	// DeviceIndexDirect addresses the device the HID interface belongs to
	// (Bluetooth pairing or own dongle). Devices behind a Bolt/Unifying
	// receiver are addressed by pairing slot 1-6 through the receiver's
	// interface.
	DeviceIndexDirect = 0xFF

	probeTimeout   = 500 * time.Millisecond // per request while probing
	setHostTimeout = 1 * time.Second        // waiting for the setHost ACK

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

// Target says how the Switch selects the device to move.
type Target int

const (
	TargetDirect Target = iota // DeviceIndexDirect
	TargetSlot                 // an explicit receiver pairing slot
	TargetKind                 // scan receiver slots for the first of Kind
)

// Switch is a parsed `hid-switch VID:PID [DEVICE_INDEX|KIND] HOST_INDEX`
// command (SPEC §5.5).
type Switch struct {
	VID, PID    uint16
	Target      Target
	DeviceIndex uint8 // TargetSlot only
	Kind        Kind  // TargetKind only
	HostIndex   uint8 // 0-2, the Easy-Switch slot minus one
}

// Parse validates a hid-switch argv (without the "hid-switch" word itself).
// Two arguments address the directly attached device; three address devices
// behind a receiver, by pairing slot (1-6) or by kind (keyboard|mouse) —
// the kind form moves every paired device of that kind.
func Parse(args []string) (*Switch, error) {
	if len(args) != 2 && len(args) != 3 {
		return nil, errors.New("usage: hid-switch VID:PID [DEVICE_INDEX|keyboard|mouse] HOST_INDEX")
	}
	vid, pid, err := parseVIDPID(args[0])
	if err != nil {
		return nil, err
	}
	host, err := strconv.ParseUint(args[len(args)-1], 10, 8)
	if err != nil || host > 2 {
		return nil, fmt.Errorf("invalid host index %q (0-2)", args[len(args)-1])
	}
	s := &Switch{
		VID:         vid,
		PID:         pid,
		Target:      TargetDirect,
		DeviceIndex: DeviceIndexDirect,
		HostIndex:   uint8(host),
	}
	if len(args) == 2 {
		return s, nil
	}
	switch args[1] {
	case "keyboard":
		s.Target, s.Kind = TargetKind, KindKeyboard
	case "mouse":
		s.Target, s.Kind = TargetKind, KindMouse
	default:
		slot, err := strconv.ParseUint(args[1], 10, 8)
		if err != nil || slot < 1 || slot > 6 {
			return nil, fmt.Errorf("invalid device selector %q (pairing slot 1-6, keyboard or mouse)", args[1])
		}
		s.Target, s.DeviceIndex = TargetSlot, uint8(slot)
	}
	return s, nil
}

func parseVIDPID(s string) (uint16, uint16, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid VID:PID %q", s)
	}
	vid, err := strconv.ParseUint(parts[0], 16, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid VID %q: %w", parts[0], err)
	}
	pid, err := strconv.ParseUint(parts[1], 16, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid PID %q: %w", parts[1], err)
	}
	return uint16(vid), uint16(pid), nil
}

// Do switches the selected device to HostIndex.
func (s *Switch) Do(ctx context.Context) error {
	c, err := openInterface(ctx, s.VID, s.PID)
	if err != nil {
		return err
	}
	defer func() {
		if err := c.dev.Close(); err != nil {
			slog.Warn("hid-switch: device close failed", "error", err)
		}
	}()

	devIdxs := []uint8{s.DeviceIndex}
	if s.Target == TargetKind {
		slots, err := c.resolveKind(ctx, s.Kind)
		if err != nil {
			return err
		}
		devIdxs = slots
	}

	var errs []error
	for _, devIdx := range devIdxs {
		fctx, cancel := context.WithTimeout(ctx, probeTimeout)
		feat, err := c.getFeature(fctx, devIdx, featChangeHost)
		cancel()
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
			"deviceIndex", devIdx, "host", s.HostIndex)
		errs = append(errs, c.setHost(ctx, feat, devIdx, s.HostIndex))
	}
	return errors.Join(errs...)
}

// Error is a HID++ error reply (report 0x8F/0xFF) from the device.
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
		return nil, fmt.Errorf("no HID interface for %04x:%04x", vid, pid)
	}

	var tried int
	var openErrs []error
	var lastErr error
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
			lastErr = err
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
	return nil, fmt.Errorf("no HID++ interface answered for %04x:%04x (%d tried)", vid, pid, tried)
}

// speaksHIDPP reports whether the interface answers HID++ requests. Any
// framed reply counts — even an error reply or "feature unsupported". The
// handshake is repeated up to probeAttempts times on silence: a dozing
// Bluetooth device may ignore the first nudges before its vendor channel
// wakes. probeTimeout bounds one nudge.
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
			break // write errors and the like are not sleep — don't retry
		}
		slog.Debug("hidpp: handshake unanswered, nudging again", "attempt", i+1)
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

// findSlots scans receiver pairing slots 1-6 for every device of the given
// kind that supports changeHost. An empty or incapable slot costs one
// probeTimeout.
func (c *conn) findSlots(ctx context.Context, kind Kind) ([]uint8, error) {
	var slots []uint8
	for slot := uint8(1); slot <= 6; slot++ {
		sctx, cancel := context.WithTimeout(ctx, probeTimeout)
		feat, err := c.getFeature(sctx, slot, featChangeHost)
		cancel()
		if err != nil || feat == 0 {
			continue
		}
		sctx, cancel = context.WithTimeout(ctx, probeTimeout)
		k, err := c.getKind(sctx, slot)
		cancel()
		if err == nil && k == kind {
			slots = append(slots, slot)
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

// request sends one HID++ request — always the long report: every HID++ 2.0
// device carries it, and changeHost targets are 2.0 by definition — and waits
// for its reply, discarding unrelated reports (notifications). Replies may
// still arrive short. An error reply comes back as *Error. The caller's ctx
// deadline bounds the single exchange.
func (c *conn) request(ctx context.Context, devIdx, featIdx, fn uint8, params ...byte) ([]byte, error) {
	report := buildReport(devIdx, featIdx, fn, params)
	slog.Debug("hidpp: request", "deviceIndex", devIdx, "featureIndex", featIdx,
		"fn", fn, "report", fmt.Sprintf("%x", report))
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
				slog.Debug("hidpp: no reply (silence, or a short-only HID++ 1.0 device ignoring the long report)")
			default:
				slog.Debug("hidpp: read failed", "error", err)
			}
			return nil, err
		}
		slog.Debug("hidpp: report in", "report", fmt.Sprintf("%x", buf[:n]))
		reply, herr := matchReply(buf[:n], devIdx, featIdx, fn)
		if reply != nil || herr != nil {
			return reply, herr
		}
	}
}

// buildReport frames a HID++ request: report ID, device index, feature
// index, function<<4|swID, then the parameters, zero-padded to 20 bytes.
func buildReport(devIdx, featIdx, fn uint8, params []byte) []byte {
	r := make([]byte, 20)
	r[0] = reportLong
	r[1] = devIdx
	r[2] = featIdx
	r[3] = fn<<4 | swID
	copy(r[4:], params)
	return r
}

// matchReply picks the reply to a request out of the input stream. It
// returns (params, nil) for a matching data reply, (nil, *Error) for a
// matching error reply, and (nil, nil) for anything else.
func matchReply(report []byte, devIdx, featIdx, fn uint8) ([]byte, error) {
	if len(report) < 5 {
		return nil, nil
	}
	if report[1] != devIdx || report[2] != featIdx || report[3] != fn<<4|swID {
		return nil, nil
	}
	switch report[0] {
	case reportShort, reportLong:
		return report[4:], nil
	case reportErrShort, reportErrLong:
		return nil, &Error{Code: report[4]}
	}
	return nil, nil
}

// PairedDevice is one occupied receiver slot.
type PairedDevice struct {
	Index uint8
	Kind  Kind
}

// Inventory is what Probe learned about one VID:PID: the kind of the device
// itself and, for receivers, the occupied pairing slots.
type Inventory struct {
	Kind   Kind
	Paired []PairedDevice
}

// Probe opens the first HID++ interface of VID:PID and reports the device
// kind; for a receiver it also scans pairing slots 1-6. Used by `detect` to
// draw the slot map (SPEC §5.5).
func Probe(ctx context.Context, vid, pid uint16) (*Inventory, error) {
	c, err := openInterface(ctx, vid, pid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.dev.Close() }()

	kctx, cancel := context.WithTimeout(ctx, probeTimeout)
	kind, err := c.getKind(kctx, DeviceIndexDirect)
	cancel()
	if err != nil {
		kind = KindUnknown
	}
	inv := &Inventory{Kind: kind}
	if kind != KindReceiver {
		return inv, nil
	}
	for slot := uint8(1); slot <= 6; slot++ {
		sctx, cancel := context.WithTimeout(ctx, probeTimeout)
		feat, err := c.getFeature(sctx, slot, featFeatureSet)
		cancel()
		if err != nil || feat == 0 {
			continue
		}
		sctx, cancel = context.WithTimeout(ctx, probeTimeout)
		k, err := c.getKind(sctx, slot)
		cancel()
		if err != nil {
			k = KindUnknown
		}
		inv.Paired = append(inv.Paired, PairedDevice{Index: slot, Kind: k})
	}
	return inv, nil
}
