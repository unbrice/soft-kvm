// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// Tests for hidpp.go. Parsing and framing are table-tested; exchanges run
// against scripted fakes inside synctest bubbles, so silent-device paths cost
// fake time, not real time. t.Run cannot be called from inside a bubble, so
// the nesting is reversed: each timed subtest wraps its body in synctest.Test.
// The I/O path itself needs hardware and is covered by the SPEC §10 checklist.

package hidpp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    *Switch
		wantErr bool
	}{
		{
			name: "device, channel from the owner",
			args: []string{"046d:c52b"},
			want: &Switch{VID: 0x046d, PID: 0xc52b},
		},
		{
			name: "device with explicit channel",
			args: []string{"046d:b35b", "host=2"},
			want: &Switch{VID: 0x046d, PID: 0xb35b, Channel: 2},
		},
		{
			name: "receiver pairing slot",
			args: []string{"046d:c548:3"},
			want: &Switch{VID: 0x046d, PID: 0xc548, Slot: 3},
		},
		{
			name: "receiver pairing slot with channel",
			args: []string{"046d:c548:3", "host=1"},
			want: &Switch{VID: 0x046d, PID: 0xc548, Slot: 3, Channel: 1},
		},
		{name: "no args", args: nil, wantErr: true},
		{name: "too many args", args: []string{"046d:b35b", "host=1", "1"}, wantErr: true},
		{name: "bad separator", args: []string{"046db35b"}, wantErr: true},
		{name: "bad vid", args: []string{"zzzz:b35b"}, wantErr: true},
		{name: "bad pid", args: []string{"046d:b35bb"}, wantErr: true},
		{name: "slot zero", args: []string{"046d:c548:0"}, wantErr: true},
		{name: "slot too high", args: []string{"046d:c548:7"}, wantErr: true},
		{name: "channel zero", args: []string{"046d:b35b", "host=0"}, wantErr: true},
		{name: "channel too high", args: []string{"046d:b35b", "host=4"}, wantErr: true},
		{name: "channel not a number", args: []string{"046d:b35b", "host=mac"}, wantErr: true},
		// The old grammar must fail loudly, never be reinterpreted: a bare
		// trailing number used to be a 0-based host index.
		{name: "old bare host index", args: []string{"046d:b35b", "1"}, wantErr: true},
		{name: "old kind form", args: []string{"046d:c548", "mouse", "0"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Parse(%v) succeeded, want error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%v): %v", c.args, err)
			}
			if *got != *c.want {
				t.Errorf("Parse(%v) = %+v, want %+v", c.args, *got, *c.want)
			}
		})
	}
}

func TestBuildReport(t *testing.T) {
	// Requests default to the 7-byte short report: every HID++ device carries
	// it, and its 3 parameter bytes are enough for everything but a hostsInfo
	// name chunk. More than 3 is an error, never a silent truncation.
	r, err := buildReport(0x02, 0x07, fnSetHost, []byte{0x01})
	if err != nil {
		t.Fatalf("buildReport: %v", err)
	}
	if len(r) != 7 {
		t.Fatalf("report length = %d, want 7", len(r))
	}
	if r[0] != reportShort || r[1] != 0x02 || r[2] != 0x07 ||
		r[3] != fnSetHost<<4|swID || r[4] != 0x01 {
		t.Errorf("report = %x", r)
	}
	for i, b := range r[5:] {
		if b != 0 {
			t.Errorf("report padding byte %d = %#x, want 0", i+5, b)
		}
	}
	if _, err := buildReport(0x02, 0x07, fnSetHost, []byte{1, 2, 3, 4}); err == nil {
		t.Error("buildReport with 4 parameter bytes succeeded, want an oversize error")
	}
}

func TestBuildLongReport(t *testing.T) {
	// The 20-byte long report fits 16 parameter bytes — a hostsInfo name
	// chunk is hostIndex + byteIndex + 14 name bytes, exactly the limit.
	params := append([]byte{0x00, 0x00}, []byte("12345678901234")...)
	r, err := buildLongReport(0x02, 0x0a, fnHostsSetName, params)
	if err != nil {
		t.Fatalf("buildLongReport: %v", err)
	}
	if len(r) != 20 {
		t.Fatalf("report length = %d, want 20", len(r))
	}
	if r[0] != reportLong || r[1] != 0x02 || r[2] != 0x0a || r[3] != fnHostsSetName<<4|swID {
		t.Errorf("report = %x", r)
	}
	if !slices.Equal(r[4:], params) {
		t.Errorf("report params = %x, want %x", r[4:], params)
	}
	if _, err := buildLongReport(0x02, 0x0a, fnHostsSetName, append(slices.Clone(params), 0x00)); err == nil {
		t.Error("buildLongReport with 17 parameter bytes succeeded, want an oversize error")
	}
}

func TestMatchReply(t *testing.T) {
	devIdx, featIdx, fn := uint8(0xFF), uint8(0x00), uint8(fnGetFeature)
	fnSw := fn<<4 | swID

	// A matching data reply yields its parameters.
	reply, err := matchReply([]byte{reportShort, devIdx, featIdx, fnSw, 0x05, 0x00, 0x00}, devIdx, featIdx, fn)
	if err != nil || len(reply) == 0 || reply[0] != 0x05 {
		t.Errorf("data reply = %x, %v; want feature index 5", reply, err)
	}

	// A matching error reply yields an *Error carrying the code. The error
	// indicator sits in the sub-ID byte of a normal short/long report.
	_, err = matchReply([]byte{reportShort, devIdx, errSubShort, featIdx, fnSw, 0x02, 0x00}, devIdx, featIdx, fn)
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != 0x02 {
		t.Errorf("error reply = %v, want *Error{Code:2}", err)
	}

	// Everything else is discarded.
	noise := [][]byte{
		{reportShort, 0x01, featIdx, fnSw, 0x05},                         // other device index
		{reportShort, devIdx, 0x07, fnSw, 0x05},                          // other feature index
		{reportShort, devIdx, featIdx, 0x9<<4 | 0x2, 0x05},               // other fn/swID
		{0x20, devIdx, featIdx, fnSw, 0x05},                              // unknown report ID
		{reportShort, devIdx, featIdx},                                   // truncated
		{reportLong, 0x01, errSubLong, featIdx, fnSw, 0x02, 0},           // error for another device
		{reportLong, devIdx, errSubLong, featIdx, 0x9<<4 | 0x2, 0x02, 0}, // error for another fn
	}
	for i, r := range noise {
		if reply, err := matchReply(r, devIdx, featIdx, fn); reply != nil || err != nil {
			t.Errorf("noise %d: got (%x, %v), want (nil, nil)", i, reply, err)
		}
	}
}

// regReceiverInfoAddr is the low address byte of the pairing-table register,
// as it appears on the wire.
const regReceiverInfoAddr = byte(regReceiverInfo & 0xFF)

func TestMatchRegisterReply(t *testing.T) {
	devIdx, addr, subreg := uint8(0xFF), regReceiverInfoAddr, uint8(0x20)

	// A matching data reply yields the register data starting at the echoed
	// sub-register (Solaar's slicing convention).
	reply, err := matchRegisterReply(
		[]byte{reportLong, devIdx, subRegReadLong, addr, subreg, 0x04, 0x08}, devIdx, subRegReadLong, addr, subreg)
	if err != nil || len(reply) < 2 || reply[0] != subreg || reply[1] != 0x04 {
		t.Errorf("data reply = %x, %v; want sub-reg echo then data", reply, err)
	}

	// A 1.0 error reply (empty slot) yields an *Error carrying the code.
	_, err = matchRegisterReply(
		[]byte{reportShort, devIdx, errSubShort, subRegReadLong, addr, 0x03}, devIdx, subRegReadLong, addr, subreg)
	var herr *Error
	if !errors.As(err, &herr) || herr.Code != 0x03 {
		t.Errorf("error reply = %v, want *Error{Code:3}", err)
	}

	// Everything else is discarded.
	noise := [][]byte{
		{reportLong, 0x01, subRegReadLong, addr, subreg, 0x04}, // other device index
		{reportLong, devIdx, subRegReadLong, addr, 0x21, 0x04}, // other sub-register
		{reportLong, devIdx, subRegReadLong, 0xB4, subreg, 0x04},
		{reportShort, devIdx, subRegReadLong, addr}, // truncated
	}
	for i, r := range noise {
		if reply, err := matchRegisterReply(r, devIdx, subRegReadLong, addr, subreg); reply != nil || err != nil {
			t.Errorf("noise %d: got (%x, %v), want (nil, nil)", i, reply, err)
		}
	}
}

func TestPairingKind(t *testing.T) {
	unifying := make([]byte, 17) // data starts at the echoed sub-register
	unifying[7] = 0x01
	bolt := make([]byte, 17)
	bolt[1] = 0x02
	cases := []struct {
		name string
		bolt bool
		data []byte
		want Kind
	}{
		{"unifying keyboard", false, unifying, KindKeyboard},
		{"bolt mouse", true, bolt, KindMouse},
		{"unifying truncated", false, unifying[:7], KindUnknown},
		{"bolt truncated", true, bolt[:1], KindUnknown},
		{"unknown nibble", false, make([]byte, 17), KindUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pairingKind(c.bolt, c.data); got != c.want {
				t.Errorf("pairingKind(%t, %x) = %v, want %v", c.bolt, c.data, got, c.want)
			}
		})
	}
}

// device would answer, and read returns one queued report — waking a parked
// read, as a real report does — or parks until ctx is done. A parked read on
// an empty queue is a silent device; inside a synctest bubble that costs fake
// time only. The buffer is 1 because conn.request always reads between
// writes, so at most one reply is ever in flight.
type reportQueue struct{ ch chan []byte }

func (q *reportQueue) push(report []byte) {
	if q.ch == nil {
		q.ch = make(chan []byte, 1)
	}
	q.ch <- report
}

// Read and Close satisfy deviceConn; embedding reportQueue promotes them
// unmodified into every fake conn below except fakeConn, which overrides
// Read to also honor readErr.
func (q *reportQueue) Read(ctx context.Context, p []byte) (int, error) {
	select {
	case r := <-q.ch: // nil channel: never ready, only ctx can fire
		return copy(p, r), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (q *reportQueue) Close() error { return nil }

// fakeConn is a scripted deviceConn: it records writes, and a write whose
// report ID is wantID queues the scripted reply. writeErr and readErr fail
// the corresponding operation.
type fakeConn struct {
	reportQueue
	writes   [][]byte
	wantID   byte
	reply    []byte
	writeErr error
	readErr  error
}

func (f *fakeConn) Write(_ context.Context, p []byte) (int, error) {
	f.writes = append(f.writes, slices.Clone(p))
	if f.writeErr == nil && f.reply != nil && p[0] == f.wantID {
		f.push(slices.Clone(f.reply))
	}
	return len(p), f.writeErr
}

func (f *fakeConn) Read(ctx context.Context, p []byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return f.reportQueue.Read(ctx, p)
}

// runSync runs fn as a subtest inside a synctest bubble. t.Run cannot be
// called from inside a bubble, so every timed subtest needs this same
// wrapping; the helper keeps that nesting out of each call site.
func runSync(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) { synctest.Test(t, fn) })
}

func TestRequest(t *testing.T) {
	devIdx, featIdx, fn := uint8(0xFF), uint8(0x00), uint8(fnGetFeature)
	fnSw := fn<<4 | swID

	// request is bounded by the ctx deadline alone: one write, then reads
	// until a matching reply or the deadline.
	request := func(t *testing.T, c *conn) ([]byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), probeTimeout)
		defer cancel()
		return c.request(ctx, devIdx, featIdx, fn)
	}

	runSync(t, "answered", func(t *testing.T) {
		fake := &fakeConn{wantID: reportShort,
			reply: []byte{reportShort, devIdx, featIdx, fnSw, 0x05}}
		reply, err := request(t, &conn{dev: fake})
		if err != nil || len(reply) == 0 || reply[0] != 0x05 {
			t.Fatalf("request = %x, %v; want feature index 5", reply, err)
		}
		if len(fake.writes) != 1 || fake.writes[0][0] != reportShort {
			t.Errorf("writes = %x, want one short report", fake.writes)
		}
	})

	runSync(t, "write error is terminal", func(t *testing.T) {
		werr := errors.New("invalid argument")
		fake := &fakeConn{writeErr: werr}
		if _, err := request(t, &conn{dev: fake}); !errors.Is(err, werr) {
			t.Fatalf("request error = %v, want %v", err, werr)
		}
		if len(fake.writes) != 1 {
			t.Errorf("%d writes, want 1", len(fake.writes))
		}
	})

	runSync(t, "hid++ error reply", func(t *testing.T) {
		fake := &fakeConn{wantID: reportShort,
			reply: []byte{reportShort, devIdx, errSubShort, featIdx, fnSw, 0x02}}
		_, err := request(t, &conn{dev: fake})
		var herr *Error
		if !errors.As(err, &herr) || herr.Code != 0x02 {
			t.Fatalf("request error = %v, want *Error{Code:2}", err)
		}
	})

	runSync(t, "no answer", func(t *testing.T) {
		fake := &fakeConn{} // nothing is ever queued: reads park until the deadline
		if _, err := request(t, &conn{dev: fake}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request error = %v, want DeadlineExceeded", err)
		}
		if len(fake.writes) != 1 {
			t.Errorf("%d writes, want 1", len(fake.writes))
		}
	})
}

// emuConn emulates a HID++ device tree well enough to exercise resolveKind:
// a directly attached device and/or occupied receiver pairing slots, each
// with a kind and a changeHost capability. Unknown device indexes and
// requests queue no reply — the read parks on the context, which a synctest
// bubble resolves in fake time. When slots is set, 0xFF also answers HID++
// 1.0 pairing-table register reads from its local "flash".
type emuConn struct {
	reportQueue
	writes [][]byte
	direct *emuDevice
	slots  map[uint8]emuDevice
}

type emuDevice struct {
	kind       Kind
	changeHost bool
}

func (e *emuConn) device(devIdx uint8) (emuDevice, bool) {
	if devIdx == DeviceIndexDirect {
		if e.direct == nil {
			return emuDevice{}, false
		}
		return *e.direct, true
	}
	dev, ok := e.slots[devIdx]
	return dev, ok
}

func (e *emuConn) Write(_ context.Context, p []byte) (int, error) {
	e.writes = append(e.writes, slices.Clone(p))
	if reply := e.answer(p); reply != nil {
		e.push(reply)
	}
	return len(p), nil
}

// answer computes the reply the device tree would send for a request report,
// or nil when it would stay silent.
func (e *emuConn) answer(report []byte) []byte {
	if report[2] == subRegReadLong && report[3] == regReceiverInfoAddr {
		return e.pairingReply(report)
	}
	devIdx, featIdx, fnSw := report[1], report[2], report[3]
	fn := fnSw >> 4
	dev, ok := e.device(devIdx)
	if !ok {
		return nil
	}
	var params []byte
	switch {
	case featIdx == 0 && fn == fnGetFeature: // IRoot
		switch feature := uint16(report[4])<<8 | uint16(report[5]); feature {
		case featFeatureSet:
			params = []byte{0x01}
		case featDeviceName:
			params = []byte{0x03}
		case featChangeHost:
			if dev.changeHost {
				params = []byte{0x04}
			} else {
				params = []byte{0x00}
			}
		default:
			params = []byte{0x00}
		}
	case featIdx == 0x03 && fn == fnGetDeviceType:
		params = []byte{byte(dev.kind)}
	}
	if params == nil {
		return nil
	}
	return append([]byte{report[0], devIdx, featIdx, fnSw}, params...)
}

// pairingReply answers a pairing-table register read from the receiver's
// local "flash": an occupied slot gets a long data reply echoing the
// sub-register (the kind nibble lands at data[1] on Bolt, data[7] on
// Unifying, past the echo), an empty slot an immediate 1.0 error. Without
// slots the interface is not a receiver and stays silent.
func (e *emuConn) pairingReply(report []byte) []byte {
	if e.slots == nil {
		return nil
	}
	subreg := report[4]
	var slot uint8
	var bolt bool
	switch {
	case subreg >= 0x20 && subreg <= 0x25: // Unifying: 0x20+slot-1
		slot = subreg - 0x20 + 1
	case subreg >= 0x51 && subreg <= 0x56: // Bolt: 0x50+slot
		slot, bolt = subreg-0x50, true
	default:
		return nil
	}
	dev, ok := e.slots[slot]
	if !ok {
		return []byte{reportShort, report[1], errSubShort, subRegReadLong, regReceiverInfoAddr, 0x03}
	}
	var nibble byte
	switch dev.kind {
	case KindKeyboard:
		nibble = 0x01
	case KindMouse:
		nibble = 0x02
	}
	data := make([]byte, 16)
	if bolt {
		data[0] = nibble
	} else {
		data[6] = nibble
	}
	return append([]byte{reportLong, report[1], subRegReadLong, regReceiverInfoAddr, subreg}, data...)
}

func TestRequestLinkDropped(t *testing.T) {
	fake := &fakeConn{readErr: syscall.EIO}
	c := &conn{dev: fake}
	_, err := c.request(t.Context(), 0xFF, 0, fnGetFeature)
	if !errors.Is(err, errLinkDropped) {
		t.Fatalf("request error = %v, want errLinkDropped", err)
	}
	if len(fake.writes) != 1 {
		t.Errorf("%d writes, want 1", len(fake.writes))
	}
}

func TestSetHost(t *testing.T) {
	featIdx, devIdx, host := uint8(0x04), uint8(0xFF), uint8(1)

	runSync(t, "link dropped after write is success", func(t *testing.T) {
		c := &conn{dev: &fakeConn{readErr: syscall.EIO}}
		if err := c.setHost(t.Context(), featIdx, devIdx, host); err != nil {
			t.Errorf("setHost = %v, want nil", err)
		}
	})

	// The silence case waits one full setHostTimeout — fake time in the bubble.
	runSync(t, "silence is success", func(t *testing.T) {
		c := &conn{dev: &fakeConn{}}
		if err := c.setHost(t.Context(), featIdx, devIdx, host); err != nil {
			t.Errorf("setHost = %v, want nil", err)
		}
	})

	runSync(t, "write error is failure", func(t *testing.T) {
		werr := errors.New("permission denied")
		c := &conn{dev: &fakeConn{writeErr: werr}}
		if err := c.setHost(t.Context(), featIdx, devIdx, host); !errors.Is(err, werr) {
			t.Errorf("setHost = %v, want %v", err, werr)
		}
	})

	runSync(t, "hid++ error reply is failure", func(t *testing.T) {
		fnSw := uint8(fnSetHost)<<4 | swID
		c := &conn{dev: &fakeConn{wantID: reportShort,
			reply: []byte{reportShort, devIdx, errSubShort, featIdx, fnSw, 0x06}}}
		var herr *Error
		if err := c.setHost(t.Context(), featIdx, devIdx, host); !errors.As(err, &herr) {
			t.Errorf("setHost = %v, want *Error", err)
		}
	})
}

func TestResolveKind(t *testing.T) {
	runSync(t, "directly attached device matches", func(t *testing.T) {
		c := &conn{dev: &emuConn{direct: &emuDevice{kind: KindMouse, changeHost: true}}}
		got, err := c.resolveKind(t.Context(), KindMouse)
		if err != nil || len(got) != 1 || got[0] != DeviceIndexDirect {
			t.Errorf("resolveKind = %v, %v; want [0xFF]", got, err)
		}
	})

	// Empty slots draw an immediate 1.0 error reply from the pairing table;
	// only matching slots cost a changeHost query over the "air".
	runSync(t, "receiver slot scan", func(t *testing.T) {
		c := &conn{dev: &emuConn{
			direct: &emuDevice{kind: KindReceiver},
			slots: map[uint8]emuDevice{
				2: {kind: KindMouse, changeHost: true},
				4: {kind: KindKeyboard, changeHost: true},
			},
		}}
		got, err := c.resolveKind(t.Context(), KindMouse)
		if err != nil || len(got) != 1 || got[0] != 2 {
			t.Errorf("resolveKind = %v, %v; want [2]", got, err)
		}
	})

	runSync(t, "kind not present", func(t *testing.T) {
		c := &conn{dev: &emuConn{
			direct: &emuDevice{kind: KindReceiver},
			slots:  map[uint8]emuDevice{4: {kind: KindKeyboard, changeHost: true}},
		}}
		if _, err := c.resolveKind(t.Context(), KindMouse); err == nil {
			t.Error("resolveKind succeeded, want error")
		}
	})
}

// nudgeConn queues its reply only once answerFrom reports have been written,
// emulating a device that sleeps through the first handshake nudges.
type nudgeConn struct {
	reportQueue
	writes     int
	answerFrom int
	reply      []byte
}

func (n *nudgeConn) Write(_ context.Context, p []byte) (int, error) {
	n.writes++
	if n.writes >= n.answerFrom {
		n.push(n.reply)
	}
	return len(p), nil
}

// featureWriteEmu fakes a featureReporter: Write fails with writeErr,
// SendFeatureReport with featureErr; both record their reports.
type featureWriteEmu struct {
	reportQueue
	writeErr, featureErr error
	writes, features     [][]byte
}

func (f *featureWriteEmu) Write(_ context.Context, p []byte) (int, error) {
	f.writes = append(f.writes, slices.Clone(p))
	return len(p), f.writeErr
}

func (f *featureWriteEmu) SendFeatureReport(p []byte) error {
	f.features = append(f.features, slices.Clone(p))
	return f.featureErr
}

func TestFeatureWriteFallback(t *testing.T) {
	notFound := errors.New("IOHIDDeviceSetReport failed: 0xe00002f0")
	report := []byte{reportShort, DeviceIndexDirect, 0, fnGetFeature<<4 | swID, 0x00, 0x01, 0x00}

	cases := []struct {
		name         string
		writeErr     error
		featureErr   error
		wantFeatures int
		wantErr      error // nil means success
	}{
		{name: "output report accepted"},
		{name: "no output report, feature accepted", writeErr: notFound, wantFeatures: 1},
		{name: "no output report, feature denied", writeErr: notFound,
			featureErr:   errors.New("IOHIDDeviceSetReport failed: 0xe00002e2"),
			wantFeatures: 1, wantErr: errors.New("IOHIDDeviceSetReport failed: 0xe00002e2")},
		{name: "other write error, no fallback", writeErr: syscall.EINVAL, wantErr: syscall.EINVAL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			emu := &featureWriteEmu{writeErr: c.writeErr, featureErr: c.featureErr}
			w := featureWriteFallback{emu}
			n, err := w.Write(t.Context(), report)
			if c.wantErr == nil {
				if err != nil || n != len(report) {
					t.Errorf("Write = %d, %v; want %d, nil", n, err, len(report))
				}
			} else if !errors.Is(err, c.wantErr) && err.Error() != c.wantErr.Error() {
				t.Errorf("Write error = %v, want %v", err, c.wantErr)
			}
			if len(emu.features) != c.wantFeatures {
				t.Errorf("%d feature reports sent, want %d", len(emu.features), c.wantFeatures)
			}
			if len(emu.writes) != 1 {
				t.Errorf("%d output writes, want 1", len(emu.writes))
			}
		})
	}
}

func TestIsReportRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"linux EPIPE", syscall.EPIPE, true},
		{"linux EINVAL", syscall.EINVAL, true},
		{"macOS report not in descriptor", errors.New("IOHIDDeviceSetReport failed: 0xe00002f0"), true},
		// The Input Monitoring gate is NOT a rejection: it wants the
		// remediation hint, not a "cannot speak HID++" classification.
		{"macOS TCC denial", errors.New("IOHIDDeviceSetReport failed: 0xe00002e2"), false},
		{"timeout", context.DeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isReportRejected(c.err); got != c.want {
				t.Errorf("isReportRejected(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestSpeaksHIDPP(t *testing.T) {
	reply := []byte{reportLong, DeviceIndexDirect, 0x00, fnGetFeature<<4 | swID, 0x01}

	runSync(t, "dozing device wakes on a later nudge", func(t *testing.T) {
		// Answers the second written report: one write per nudge.
		c := &conn{dev: &nudgeConn{answerFrom: 2, reply: reply}}
		if err := c.speaksHIDPP(t.Context()); err != nil {
			t.Errorf("speaksHIDPP = %v, want nil", err)
		}
	})

	runSync(t, "gives up after the attempts", func(t *testing.T) {
		c := &conn{dev: &nudgeConn{answerFrom: 99, reply: reply}}
		if err := c.speaksHIDPP(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("speaksHIDPP = %v, want DeadlineExceeded", err)
		}
	})

	runSync(t, "write error is not retried", func(t *testing.T) {
		werr := errors.New("permission denied")
		c := &conn{dev: &fakeConn{writeErr: werr}}
		if err := c.speaksHIDPP(t.Context()); !errors.Is(err, werr) {
			t.Errorf("speaksHIDPP = %v, want %v", err, werr)
		}
	})
}

// Without an explicit host=N and without a channel published by the owner
// there is nowhere safe to send a peripheral, so Do must refuse rather than
// guess — and it must refuse before touching any hardware.
func TestSwitchNeedsAChannel(t *testing.T) {
	sw := &Switch{VID: 0x046d, PID: 0xc52b}
	err := sw.Do(context.Background(), 0)
	if err == nil {
		t.Fatal("Do with no channel succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "no target host channel") {
		t.Errorf("Do error = %v, want it to name the missing channel", err)
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in       string
		vid, pid uint16
		slot     uint8
	}{
		{"046d:c52b", 0x046d, 0xc52b, 0},
		{"046d:c52b:2", 0x046d, 0xc52b, 2},
		{" 046d:c52b:6 ", 0x046d, 0xc52b, 6},
	}
	for _, c := range cases {
		vid, pid, slot, err := ParseTarget(c.in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", c.in, err)
			continue
		}
		if vid != c.vid || pid != c.pid || slot != c.slot {
			t.Errorf("ParseTarget(%q) = %04x:%04x:%d, want %04x:%04x:%d",
				c.in, vid, pid, slot, c.vid, c.pid, c.slot)
		}
	}
	for _, in := range []string{"", "046d", "046d:c52b:2:3", "zz:c52b", "046d:c52b:0", "046d:c52b:7"} {
		if _, _, _, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) succeeded, want error", in)
		}
	}
}
