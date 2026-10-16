// Package reexec 提供进程自重启（syscall.Exec 替换当前镜像）。
package reexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Self 用当前可执行文件与原 argv/environ 执行 Exec，成功时不返回。
func Self() error {
	exe, err := Resolve()
	if err != nil {
		return err
	}
	return Path(exe)
}

// Path 用指定可执行路径 Exec 当前进程（成功时不返回）。
// 自更新场景应传入替换后的目标路径，避免 /proc/self/exe 仍指向已删的 .bak。
func Path(exe string) error {
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return fmt.Errorf("可执行路径为空")
	}
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("可执行文件不可用: %w", err)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// Resolve 解析当前进程对应的可执行路径。
// 自更新时可能先把旧文件改名为 .bak 再删除，/proc/self/exe 经 EvalSymlinks
// 会落到不存在的 .bak；此时回退到去 .bak 后缀或未解析路径。
func Resolve() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位可执行文件失败: %w", err)
	}
	return resolvePath(exe)
}

func resolvePath(exe string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		// 非「文件不存在」时仍尝试其它回退，但保留原错误上下文
		if alt := fallbackExe(exe); alt != "" {
			return alt, nil
		}
		return "", fmt.Errorf("解析可执行路径失败: %w", err)
	}
	if alt := fallbackExe(exe); alt != "" {
		return alt, nil
	}
	return "", fmt.Errorf("解析可执行路径失败: lstat %s: no such file or directory", exe)
}

func fallbackExe(exe string) string {
	// 旧二进制被改名为 *.bak 后删除：进程仍可能报告 .bak 路径
	if strings.HasSuffix(exe, ".bak") {
		candidate := strings.TrimSuffix(exe, ".bak")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	if st, err := os.Stat(exe); err == nil && !st.IsDir() {
		return exe
	}
	return ""
}
