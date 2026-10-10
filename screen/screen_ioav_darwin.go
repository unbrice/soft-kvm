// SPDX-FileCopyrightText: 2026 Brice Arnould
//
// SPDX-License-Identifier: MIT OR Apache-2.0

// screen_ioav_darwin.go: DDC/CI over IOKit's IOAVService, the private I²C
// channel Apple Silicon gives an external display, reached through purego so
// the binary stays cgo-free (SPEC §5.6).

package screen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ebitengine/purego"
)

const cfStringEncodingUTF8 = 0x08000100

// The CoreFoundation and IOKit calls, bound on first use by loadIOKit.
var (
	cfRelease                       func(cf uintptr)
	cfGetTypeID                     func(cf uintptr) uint
	cfStringGetTypeID               func() uint
	cfStringCreateWithCString       func(alloc uintptr, s string, encoding uint32) uintptr
	cfStringGetCString              func(s uintptr, buf *byte, size int, encoding uint32) uint8
	ioServiceMatching               func(class string) uintptr
	ioServiceGetMatchingServices    func(mainPort uint32, matching uintptr, it *uint32) int32
	ioIteratorNext                  func(it uint32) uint32
	ioObjectRelease                 func(obj uint32) int32
	ioRegistryEntryCreateCFProperty func(entry uint32, key, alloc uintptr, options uint32) uintptr
	ioAVServiceCreateWithService    func(alloc uintptr, service uint32) uintptr
	ioAVServiceReadI2C              func(svc uintptr, chip, offset uint32, buf *byte, size uint32) int32
	ioAVServiceWriteI2C             func(svc uintptr, chip, offset uint32, buf *byte, size uint32) int32
)

// loadIOKit binds the calls above, once. IOAVService is private API: a macOS
// that lacks it fails here, with the missing symbol named.
var loadIOKit = sync.OnceValue(func() error {
	const (
		cfPath    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
		iokitPath = "/System/Library/Frameworks/IOKit.framework/IOKit"
	)
	cf, err := purego.Dlopen(cfPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load CoreFoundation: %w", err)
	}
	iokit, err := purego.Dlopen(iokitPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load IOKit: %w", err)
	}
	for _, f := range []struct {
		fn   any
		lib  uintptr
		name string
	}{
		{&cfRelease, cf, "CFRelease"},
		{&cfGetTypeID, cf, "CFGetTypeID"},
		{&cfStringGetTypeID, cf, "CFStringGetTypeID"},
		{&cfStringCreateWithCString, cf, "CFStringCreateWithCString"},
		{&cfStringGetCString, cf, "CFStringGetCString"},
		{&ioServiceMatching, iokit, "IOServiceMatching"},
		{&ioServiceGetMatchingServices, iokit, "IOServiceGetMatchingServices"},
		{&ioIteratorNext, iokit, "IOIteratorNext"},
		{&ioObjectRelease, iokit, "IOObjectRelease"},
		{&ioRegistryEntryCreateCFProperty, iokit, "IORegistryEntryCreateCFProperty"},
		{&ioAVServiceCreateWithService, iokit, "IOAVServiceCreateWithService"},
		{&ioAVServiceReadI2C, iokit, "IOAVServiceReadI2C"},
		{&ioAVServiceWriteI2C, iokit, "IOAVServiceWriteI2C"},
	} {
		sym, err := purego.Dlsym(f.lib, f.name)
		if err != nil {
			return fmt.Errorf("look up %s: %w", f.name, err)
		}
		purego.RegisterFunc(f.fn, sym)
	}
	return nil
})

// avService is an open IOAVService on one display.
type avService uintptr

// externalServices opens an IOAVService on each external display: the
// DCPAVServiceProxy entries whose Location is "External", which leaves the
// built-in panel out. The caller closes them.
func externalServices() ([]avService, error) {
	if err := loadIOKit(); err != nil {
		return nil, err
	}
	var it uint32
	// IOServiceGetMatchingServices takes the matching dictionary's reference.
	if kr := ioServiceGetMatchingServices(0, ioServiceMatching("DCPAVServiceProxy"), &it); kr != 0 {
		return nil, fmt.Errorf("IOServiceGetMatchingServices: %#x", uint32(kr))
	}
	defer ioObjectRelease(it)
	key := cfStringCreateWithCString(0, "Location", cfStringEncodingUTF8)
	defer cfRelease(key)
	var svcs []avService
	for entry := ioIteratorNext(it); entry != 0; entry = ioIteratorNext(it) {
		if registryString(entry, key) == "External" {
			if ref := ioAVServiceCreateWithService(0, entry); ref != 0 {
				svcs = append(svcs, avService(ref))
			}
		}
		ioObjectRelease(entry)
	}
	return svcs, nil
}

// registryString reads a string property off an IORegistry entry: "" when it
// is missing or not a string.
func registryString(entry uint32, key uintptr) string {
	v := ioRegistryEntryCreateCFProperty(entry, key, 0, 0)
	if v == 0 {
		return ""
	}
	defer cfRelease(v)
	if cfGetTypeID(v) != cfStringGetTypeID() {
		return ""
	}
	buf := make([]byte, 64)
	cfStringGetCString(v, &buf[0], len(buf), cfStringEncodingUTF8)
	s, _, _ := strings.Cut(string(buf), "\x00")
	return s
}

func (s avService) close() { cfRelease(uintptr(s)) }

// write sends msg from src, twice, 10 ms apart, as every Apple Silicon DDC
// tool does: here, a lone Get VCP request drew only a null reply. One send
// that goes through is enough.
func (s avService) write(ctx context.Context, src uint8, msg []byte) error {
	sent, r := false, int32(0)
	for range 2 {
		if err := sleep(ctx, 10*time.Millisecond); err != nil {
			return err
		}
		if r = ioAVServiceWriteI2C(uintptr(s), ddcAddr, uint32(src), &msg[0], uint32(len(msg))); r == 0 {
			sent = true
		}
	}
	if !sent {
		return fmt.Errorf("IOAVServiceWriteI2C: %#x", uint32(r))
	}
	return nil
}

func (s avService) read(chip, offset uint32, buf []byte) error {
	if r := ioAVServiceReadI2C(uintptr(s), chip, offset, &buf[0], uint32(len(buf))); r != 0 {
		return fmt.Errorf("IOAVServiceReadI2C: %#x", uint32(r))
	}
	return nil
}

// edid reads the display's EDID base block.
func (s avService) edid() ([]byte, error) {
	buf := make([]byte, 128)
	return buf, s.read(edidAddr, 0, buf)
}

// setVCP writes value to vcp, sent from src: 0x51 normally, 0x50 for LG's
// side channel.
func (s avService) setVCP(ctx context.Context, src, vcp uint8, value uint16) error {
	return s.write(ctx, src, setVCPRequest(src, vcp, value))
}

// getTries is how often getVCP asks before the monitor counts as silent.
// DDC/CI lets a busy monitor send a null reply too, and ddcutil retries
// those: one taken at first sight is a false receipt.
const getTries = 3

// getVCP reads vcp's current value. A failed exchange or an empty reply
// wraps errNoReply: the monitor no longer answers on this input.
func (s avService) getVCP(ctx context.Context, vcp uint8) (uint16, error) {
	var (
		v   uint16
		err error
	)
	for i := range getTries {
		if i > 0 {
			if err := sleep(ctx, ddcDelay); err != nil {
				return 0, err
			}
		}
		v, err = s.exchange(ctx, vcp)
		if !errors.Is(err, errNoReply) {
			return v, err
		}
	}
	return 0, err
}

func (s avService) exchange(ctx context.Context, vcp uint8) (uint16, error) {
	if err := s.write(ctx, ddcHost, getVCPRequest(ddcHost, vcp)); err != nil {
		if ctx.Err() != nil {
			return 0, err
		}
		return 0, errors.Join(errNoReply, err)
	}
	if err := sleep(ctx, ddcDelay); err != nil {
		return 0, err
	}
	reply := make([]byte, 11)
	if err := s.read(ddcAddr, ddcHost, reply); err != nil {
		return 0, errors.Join(errNoReply, err)
	}
	return parseVCPReply(reply, vcp)
}

// ddcDelay covers what DDC/CI asks of the host: 40 ms before reading a
// reply, 50 ms after a write before the next message.
const ddcDelay = 50 * time.Millisecond

// sleep waits d unless ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
