// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// debug.go: the hidden `mac-debug` command — a raw hostsInfo (0x1815)
// transcript for one device: both feature indexes, every host record and
// friendly name, and optionally a guarded write of a name at the current
// host, then a read-back. The transcript settles the wire layouts and the
// per-transport write framing the host-name design stands on, so raw report
// bytes are printed beside their decoding. Only fn 0, 1 and 3 of 0x1815 are
// ever called, plus fn 4 under a write flag and only at the current host —
// fn 5 renumbers channels, fn 6 unpairs, and an empty fn 4 chunk wipes
// (SPEC §5.5).

package hidpp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// HostsDebugOptions selects HostsDebug's write phase; the zero value reads
// only. Set is the name to write at the current host, truncated to the
// device's nameMaxLen; WriteBack rewrites the name already stored there.
// Long frames each write as one 0x11 report per 14-byte chunk instead of one
// 0x10 report per byte.
type HostsDebugOptions struct {
	WriteBack bool
	Set       string
	Long      bool
}

// HostsDebug prints the mac-debug transcript for VID:PID to w — the pairing
// slot's device, or the directly attached one when slot is 0. Every request
// is printed with its raw bytes and its elapsed time.
func HostsDebug(ctx context.Context, w io.Writer, vid, pid uint16, slot uint8, opts HostsDebugOptions) error {
	c, err := openInterface(ctx, vid, pid)
	if err != nil {
		return err
	}
	defer func() { _ = c.dev.Close() }()
	_, _ = fmt.Fprintf(w, "interface %s usagePage=%04x usage=%04x\n",
		c.info.Path, c.info.UsagePage, c.info.Usage)

	devIdx := uint8(DeviceIndexDirect)
	if slot != 0 {
		devIdx = slot
	}
	return (&hostsDebug{c: c, w: w, devIdx: devIdx}).run(ctx, opts)
}

// hostsDebug carries the transcript state: the open interface, where to
// print, the device being addressed and its two feature indexes.
type hostsDebug struct {
	c              *conn
	w              io.Writer
	devIdx         uint8
	featChangeHost uint8 // resolved 0x1814 index
	featHostsInfo  uint8 // resolved 0x1815 index
}

// run prints the read transcript, then — under a write flag — the write and
// the read-back that is its verdict.
func (d *hostsDebug) run(ctx context.Context, opts HostsDebugOptions) error {
	if opts.WriteBack && opts.Set != "" {
		return errors.New("--write-back and --set are mutually exclusive")
	}
	if err := d.resolveFeatures(ctx); err != nil {
		return err
	}
	if d.featChangeHost == 0 {
		_, _ = fmt.Fprintln(d.w, "no changeHost (0x1814) — nothing numbers the hosts")
		return nil
	}
	if d.featHostsInfo == 0 {
		_, _ = fmt.Fprintln(d.w, "no hostsInfo")
		return nil
	}
	_, currHost, hosts, err := d.readAll(ctx)
	if err != nil {
		return err
	}
	if !opts.WriteBack && opts.Set == "" {
		return nil
	}

	rec := hosts[currHost]
	if !rec.ok || rec.nameMaxLen == 0 {
		return fmt.Errorf("cannot write: the record of the current host (index %d) did not read", currHost)
	}
	var target []byte
	if opts.WriteBack {
		if rec.name == "" {
			_, _ = fmt.Fprintln(d.w, "blank, nothing to write back")
			return nil
		}
		target = []byte(rec.name)
	} else {
		target = []byte(opts.Set)
	}
	if len(target) > int(rec.nameMaxLen) {
		target = target[:rec.nameMaxLen]
		_, _ = fmt.Fprintf(d.w, "truncated to nameMaxLen %d: %q\n", rec.nameMaxLen, target)
	}
	d.writeName(ctx, currHost, target, opts.Long)

	// Items 3-5 again: the read-back is the verdict on the write.
	_, currHost, hosts, err = d.readAll(ctx)
	if err != nil {
		return err
	}
	if got := hosts[currHost].name; got == string(target) {
		_, _ = fmt.Fprintf(d.w, "verdict: channel %d name is %q, as written\n", currHost+1, got)
	} else {
		_, _ = fmt.Fprintf(d.w, "verdict: MISMATCH — wrote %q, read back %q at channel %d\n",
			target, got, currHost+1)
	}
	return nil
}

// resolveFeatures prints both feature indexes. The first request to a device
// is nudged: a dozing one wakes on it (cold RF is ~700ms, past one
// probeTimeout). Index 0 means the device does not have the feature.
func (d *hostsDebug) resolveFeatures(ctx context.Context) error {
	var chID, hiID uint16 = featChangeHost, featHostsInfo
	reply, err := d.callNudged(ctx, "getFeature(0x1814 changeHost)",
		false, 0, fnGetFeature, byte(chID>>8), byte(chID))
	if err != nil {
		return err
	}
	if len(reply) == 0 {
		return errors.New("empty getFeature reply")
	}
	d.featChangeHost = reply[0]
	_, _ = fmt.Fprintf(d.w, "  index %d\n", d.featChangeHost)

	reply, err = d.call(ctx, "getFeature(0x1815 hostsInfo)",
		false, 0, fnGetFeature, byte(hiID>>8), byte(hiID))
	if err != nil {
		return err
	}
	if len(reply) == 0 {
		return errors.New("empty getFeature reply")
	}
	d.featHostsInfo = reply[0]
	_, _ = fmt.Fprintf(d.w, "  index %d\n", d.featHostsInfo)
	return nil
}

// hostRecord is one hostsInfo host record (fn 1) plus its friendly name
// (fn 3), as read. ok is false when the record's own request failed.
type hostRecord struct {
	status     uint8
	busType    uint8
	numPages   uint8
	nameLen    uint8
	nameMaxLen uint8
	name       string
	ok         bool
}

// readAll prints 0x1814 getHostInfo, 0x1815 getFeatureInfo (raw only — its
// layout is one of the things the transcript settles), then each host's
// record and name. A per-host failure is printed and left as a zero record,
// not fatal: the transcript should show as much as answers.
func (d *hostsDebug) readAll(ctx context.Context) (nbHost, currHost uint8, hosts []hostRecord, err error) {
	reply, err := d.call(ctx, "0x1814 fn 0 getHostInfo", false, d.featChangeHost, fnGetHostInfo)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(reply) < 2 {
		return 0, 0, nil, fmt.Errorf("short getHostInfo reply: % x", reply)
	}
	nbHost, currHost = reply[0], reply[1]
	_, _ = fmt.Fprintf(d.w, "  nbHost=%d currHost=%d (channel %d)\n", nbHost, currHost, currHost+1)
	if currHost >= nbHost {
		return 0, 0, nil, fmt.Errorf("getHostInfo: current host %d outside the %d it reports", currHost, nbHost)
	}

	if _, err := d.call(ctx, "0x1815 fn 0 getFeatureInfo", false, d.featHostsInfo, fnHostsFeatureInfo); err != nil {
		return 0, 0, nil, err
	}

	hosts = make([]hostRecord, nbHost)
	for i := range hosts {
		hosts[i] = d.readHost(ctx, uint8(i))
	}
	return nbHost, currHost, hosts, nil
}

// readHost prints one host's record and name and returns both.
func (d *hostsDebug) readHost(ctx context.Context, host uint8) hostRecord {
	var rec hostRecord
	reply, err := d.call(ctx, fmt.Sprintf("0x1815 fn 1 getHostInfo(%d)", host),
		false, d.featHostsInfo, fnHostsGetInfo, host)
	if err != nil {
		return rec
	}
	if len(reply) < 6 {
		_, _ = fmt.Fprintf(d.w, "  short record: % x\n", reply)
		return rec
	}
	// The record is hostIndex, status, busType, numPages, nameLen, nameMaxLen.
	rec.status, rec.busType, rec.numPages, rec.nameLen, rec.nameMaxLen =
		reply[1], reply[2], reply[3], reply[4], reply[5]
	rec.ok = true
	_, _ = fmt.Fprintf(d.w, "  status=%d busType=%d numPages=%d nameLen=%d nameMaxLen=%d\n",
		rec.status, rec.busType, rec.numPages, rec.nameLen, rec.nameMaxLen)

	// The name reply is hostIndex, byteIndex, then at most 14 name bytes;
	// only nameLen bytes are meaningful and the padding is not part of the
	// name. One chunk is read even when nameLen is 0, to show what a blank
	// slot answers.
	var name []byte
	for off := 0; off < max(int(rec.nameLen), 1); off += 14 {
		reply, err := d.call(ctx, fmt.Sprintf("0x1815 fn 3 getHostFriendlyName(%d, %d)", host, off),
			false, d.featHostsInfo, fnHostsGetName, host, byte(off))
		if err != nil {
			return rec
		}
		if len(reply) < 2 {
			_, _ = fmt.Fprintf(d.w, "  short name reply: % x\n", reply)
			return rec
		}
		if want := min(14, int(rec.nameLen)-off); want > 0 {
			name = append(name, reply[2:min(2+want, len(reply))]...)
		}
	}
	rec.name = string(name)
	_, _ = fmt.Fprintf(d.w, "  name %q\n", rec.name)
	return rec
}

// writeName writes target at the current host — fn 4 at currHost, never an
// empty chunk: that write is the wipe. One line per write call: framing,
// byteIndex, chunk, reply or error. A failed write stops the phase; the
// read-back that follows reports whatever state the device is left in.
func (d *hostsDebug) writeName(ctx context.Context, currHost uint8, target []byte, long bool) {
	if len(target) == 0 {
		_, _ = fmt.Fprintln(d.w, "nothing to write — an empty chunk is the wipe")
		return
	}
	if long {
		_, _ = fmt.Fprintf(d.w, "writing %q at host index %d: one 0x11 report per 14-byte chunk\n",
			target, currHost)
		for off := 0; off < len(target); off += 14 {
			chunk := target[off:min(off+14, len(target))]
			params := append([]byte{currHost, byte(off)}, chunk...)
			_, err := d.call(ctx, fmt.Sprintf("0x1815 fn 4 setHostFriendlyName(%d, %d, %q)", currHost, off, chunk),
				true, d.featHostsInfo, fnHostsSetName, params...)
			if err != nil {
				_, _ = fmt.Fprintf(d.w, "  write stopped at byteIndex %d: %v\n", off, err)
				return
			}
		}
		return
	}
	_, _ = fmt.Fprintf(d.w, "writing %q at host index %d: one 0x10 report per byte\n", target, currHost)
	for off, b := range target {
		_, err := d.call(ctx, fmt.Sprintf("0x1815 fn 4 setHostFriendlyName(%d, %d, %q)", currHost, off, string(b)),
			false, d.featHostsInfo, fnHostsSetName, currHost, byte(off), b)
		if err != nil {
			_, _ = fmt.Fprintf(d.w, "  write stopped at byteIndex %d: %v\n", off, err)
			return
		}
	}
}

// call performs one request, prints it with its raw bytes and its elapsed
// time, and returns the reply parameters. Each exchange gets probeTimeout,
// never the caller's deadline: one run is many exchanges.
func (d *hostsDebug) call(ctx context.Context, label string, long bool, featIdx, fn uint8, params ...byte) ([]byte, error) {
	build := buildReport
	if long {
		build = buildLongReport
	}
	report, err := build(d.devIdx, featIdx, fn, params)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	reply, rerr := d.c.exchange(rctx, report, func(r []byte) ([]byte, error) {
		return matchReply(r, d.devIdx, featIdx, fn)
	})
	elapsed := time.Since(start).Round(time.Millisecond)
	if rerr != nil {
		_, _ = fmt.Fprintf(d.w, "%s  tx=% x  error: %v  (%s)\n", label, report, rerr, elapsed)
		return nil, rerr
	}
	_, _ = fmt.Fprintf(d.w, "%s  tx=% x  rx=% x  (%s)\n", label, report, reply, elapsed)
	return reply, nil
}

// callNudged repeats a call on silence: a dozing wireless device wakes on
// the first requests. Only deadline misses retry — an error reply or a write
// failure is an answer, not sleep.
func (d *hostsDebug) callNudged(ctx context.Context, label string, long bool, featIdx, fn uint8, params ...byte) ([]byte, error) {
	var reply []byte
	var err error
	for i := 0; i < probeAttempts; i++ {
		reply, err = d.call(ctx, label, long, featIdx, fn, params...)
		if !errors.Is(err, context.DeadlineExceeded) {
			return reply, err
		}
		_, _ = fmt.Fprintln(d.w, "  (silence, nudging again)")
	}
	return nil, err
}
