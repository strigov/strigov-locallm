package supervisor

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	jobMu sync.Mutex
	job   windows.Handle // держится открытым до выхода процесса (§7.1)
)

// jobFlags — §7.1 спецификации v2. KILL_ON_JOB_CLOSE: при гибели хоста ядро закрывает последний хэндл job и
// завершает всех потомков. BREAKAWAY_OK: выйти из job может только процесс, созданный с CREATE_BREAKAWAY_FROM_JOB;
// так запускает браузер, Блокнот и Проводник только internal/desktop. llama-server и nvidia-smi этот флаг не ставят
// и умирают вместе с хостом. Тихий выход из job (SILENT_BREAKAWAY_OK) не ставится никогда.
const jobFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK

// EnterJob помещает текущий процесс в Job Object с jobFlags. Хэндл не наследуемый. Повторный вызов — no-op.
func EnterJob() error {
	jobMu.Lock()
	defer jobMu.Unlock()
	if job != 0 {
		return nil
	}
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	// new() не гарантирует размещение в куче, а KeepAlive не мешает переносу стека: адрес, превращённый в uintptr,
	// мог бы устареть. Объект закрепляется runtime.Pinner на время вызова (как в спайке S9).
	info := new(windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION)
	info.BasicLimitInformation.LimitFlags = jobFlags
	var pin runtime.Pinner
	pin.Pin(info)
	_, err = windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(info)), uint32(unsafe.Sizeof(*info)))
	pin.Unpin()
	if err != nil {
		windows.CloseHandle(h)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	if err := windows.AssignProcessToJobObject(h, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(h)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	job = h
	return nil
}

// jobLimitFlags — LimitFlags job процесса (тест §7.1).
func jobLimitFlags() (uint32, error) {
	jobMu.Lock()
	defer jobMu.Unlock()
	if job == 0 {
		return 0, errors.New("the process is not in a job")
	}
	info := new(windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION)
	var pin runtime.Pinner
	pin.Pin(info)
	err := windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(info)), uint32(unsafe.Sizeof(*info)), nil)
	pin.Unpin()
	return info.BasicLimitInformation.LimitFlags, err
}
