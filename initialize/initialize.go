package initialize

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"unsafe"
)

// 前期连接
const (
	lolUxProcessName                  = "LeagueClientUx.exe"
	ProcessCommandLineInformation     = 60
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
)

var (
	lolCommandlineReg             = regexp.MustCompile(`--remoting-auth-token=(.+?)" ".*?--app-port=(\d+)"`)
	modntdll                      = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryInformationProcess = modntdll.NewProc("NtQueryInformationProcess")
)

type UNICODE_STRING struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func getProcessPidByName(name string) ([]int, error) {
	cmd := exec.Command("wmic", "process", "where", fmt.Sprintf("name like '%%%s%%'", name), "get", "processid")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	// 将输出按行分割
	lines := strings.Split(string(output), "\n")
	var pids []int
	// 处理每行输出
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 0 {
			// 转换为数字并添加到结果中
			pid, err := strconv.Atoi(trimmed)
			if err == nil {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) == 0 {
		return nil, errors.New(fmt.Sprintf("未找到进程:%s", name))
	}
	return pids, nil
}

func GetProcessCommandLine(pid uint32) (string, error) {
	// Open the process with PROCESS_QUERY_LIMITED_INFORMATION
	handle, err := windows.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("failed to open process: %v", err)
	}
	defer windows.CloseHandle(handle)

	// Query the buffer length for the command line information
	var bufLen uint32
	r1, _, err := procNtQueryInformationProcess.Call(
		uintptr(handle),
		uintptr(ProcessCommandLineInformation),
		0,
		0,
		uintptr(unsafe.Pointer(&bufLen)),
	)

	// Allocate buffer to hold command line information
	buffer := make([]byte, bufLen)
	r1, _, err = procNtQueryInformationProcess.Call(
		uintptr(handle),
		uintptr(ProcessCommandLineInformation),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(bufLen),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if r1 != 0 {
		return "", fmt.Errorf("NtQueryInformationProcess failed, error code: %v", err)
	}
	// Check if the buffer length is valid and non-zero
	if bufLen == 0 {
		return "", fmt.Errorf("No command line found for process %d", pid)
	}

	// Parse the buffer into a UNICODE_STRING
	ucs := (*UNICODE_STRING)(unsafe.Pointer(&buffer[0]))
	cmdLine := windows.UTF16ToString((*[1 << 20]uint16)(unsafe.Pointer(ucs.Buffer))[:ucs.Length/2])

	return cmdLine, nil
}

func NewCertificate() (token string, port int) {
	pids, err := getProcessPidByName(lolUxProcessName)
	if err != nil {
		//fmt.Println("无法获取到LOL客户端pid")
		return
	}
	cmdLine, err := GetProcessCommandLine(uint32(pids[0]))
	if err != nil {
		fmt.Printf("无法获取进程命令行: %v\n", err)
		return
	}
	btsChunk := lolCommandlineReg.FindSubmatch([]byte(cmdLine))
	if len(btsChunk) < 3 {
		fmt.Println("格式错误")
		return
	}
	token = string(btsChunk[1])
	port, _ = strconv.Atoi(string(btsChunk[2]))
	return token, port
}
