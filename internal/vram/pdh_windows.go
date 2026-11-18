package vram

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const counterPath = `\GPU Adapter Memory(*)\Dedicated Usage`

const (
	pdhFmtLarge         = 0x00000400
	pdhMoreData         = 0x800007D2
	pdhCstatusValidData = 0x00000000
	pdhCstatusNewData   = 0x00000001
)

var (
	modpdh                           = windows.NewLazySystemDLL("pdh.dll") // только %SystemRoot%\System32
	procPdhOpenQueryW                = modpdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW        = modpdh.NewProc("PdhAddEnglishCounterW")
	procPdhAddCounterW               = modpdh.NewProc("PdhAddCounterW") // только контрольный замер S3
	procPdhCollectQueryData          = modpdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW = modpdh.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery                = modpdh.NewProc("PdhCloseQuery")
)

type pdhStatus uint32

func (s pdhStatus) Error() string { return fmt.Sprintf("PDH status 0x%08X", uint32(s)) }

// pdhValue — PDH_FMT_COUNTERVALUE на amd64: DWORD CStatus, выравнивание до 8, union 8 байт.
type pdhValue struct {
	CStatus uint32
	_       uint32
	Large   int64
}

// pdhItem — PDH_FMT_COUNTERVALUE_ITEM_W (24 байта).
type pdhItem struct {
	Name  *uint16
	Value pdhValue
}

// instance — экземпляр счётчика (адаптер): имя и байты.
type instance struct {
	Name  string
	Bytes int64
}

// pdhProbe — один query на процесс, доступ под мьютексом (§8.2).
type pdhProbe struct {
	mu      sync.Mutex
	query   uintptr
	counter uintptr
}

func openPDH() (Probe, error) {
	p, err := openPDHWith(procPdhAddEnglishCounterW)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// openPDHWith открывает query, добавляет счётчик функцией add и делает пробный замер (≥ 1 экземпляр).
func openPDHWith(add *windows.LazyProc) (*pdhProbe, error) {
	if err := modpdh.Load(); err != nil {
		return nil, fmt.Errorf("pdh.dll: %w", err)
	}
	for _, proc := range []*windows.LazyProc{procPdhOpenQueryW, add, procPdhCollectQueryData, procPdhGetFormattedCounterArrayW, procPdhCloseQuery} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("pdh.dll: %w", err)
		}
	}
	p := &pdhProbe{}
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&p.query))); r != 0 {
		return nil, fmt.Errorf("PdhOpenQueryW: %w", pdhStatus(r))
	}
	path, err := windows.UTF16PtrFromString(counterPath)
	if err != nil {
		p.Close()
		return nil, err
	}
	if r, _, _ := add.Call(p.query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&p.counter))); r != 0 {
		p.Close()
		return nil, fmt.Errorf("%s(%s): %w", add.Name, counterPath, pdhStatus(r))
	}
	inst, err := p.instances()
	if err != nil {
		p.Close()
		return nil, err
	}
	if len(inst) == 0 {
		p.Close()
		return nil, errors.New("PDH counter has no GPU Adapter Memory instances")
	}
	return p, nil
}

func (p *pdhProbe) Name() string { return "pdh" }

// UsageMiB — сумма Dedicated Usage по всем экземплярам (байты) / 2^20 (§8.2).
func (p *pdhProbe) UsageMiB(context.Context) (int64, error) {
	inst, err := p.instances()
	if err != nil {
		return 0, err
	}
	if len(inst) == 0 {
		return 0, errors.New("PDH counter has no instances")
	}
	var sum int64
	for _, i := range inst {
		sum += i.Bytes
	}
	return sum >> 20, nil
}

// instances — один PdhCollectQueryData и чтение массива (счётчик мгновенный, §8.2; проверяет S3).
func (p *pdhProbe) instances() ([]instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.query == 0 {
		return nil, errors.New("PDH query is closed")
	}
	if r, _, _ := procPdhCollectQueryData.Call(p.query); r != 0 {
		return nil, fmt.Errorf("PdhCollectQueryData: %w", pdhStatus(r))
	}
	itemSize := uint32(unsafe.Sizeof(pdhItem{}))
	for attempt := 0; attempt < 3; attempt++ {
		var size, count uint32
		r, _, _ := procPdhGetFormattedCounterArrayW.Call(p.counter, pdhFmtLarge,
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
		if r == 0 || (r == pdhMoreData && size == 0) {
			return nil, nil
		}
		if r != pdhMoreData {
			return nil, fmt.Errorf("PdhGetFormattedCounterArrayW: %w", pdhStatus(r))
		}
		buf := make([]pdhItem, (size+itemSize-1)/itemSize)
		r, _, _ = procPdhGetFormattedCounterArrayW.Call(p.counter, pdhFmtLarge,
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
		if r == pdhMoreData {
			continue // экземпляров стало больше между вызовами
		}
		if r != 0 {
			return nil, fmt.Errorf("PdhGetFormattedCounterArrayW: %w", pdhStatus(r))
		}
		if int(count) > len(buf) {
			return nil, fmt.Errorf("PdhGetFormattedCounterArrayW: %d items do not fit %d", count, len(buf))
		}
		out := make([]instance, 0, count)
		for _, it := range buf[:count] {
			name := windows.UTF16PtrToString(it.Name)
			if it.Value.CStatus != pdhCstatusValidData && it.Value.CStatus != pdhCstatusNewData {
				return nil, fmt.Errorf("PDH instance %q: %w", name, pdhStatus(it.Value.CStatus))
			}
			out = append(out, instance{Name: name, Bytes: it.Value.Large})
		}
		return out, nil
	}
	return nil, errors.New("PdhGetFormattedCounterArrayW: instance list kept growing")
}

// Close закрывает query; хост держит единственный query до выхода.
func (p *pdhProbe) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.query != 0 {
		procPdhCloseQuery.Call(p.query)
		p.query, p.counter = 0, 0
	}
}
