// Package reexec 提供进程自重启（syscall.Exec 替换当前镜像）。
package reexec

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Self 用当前可执行文件与原 argv/environ 执行 Exec，成功时不返回。
func Self() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件失败: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("解析可执行路径失败: %w", err)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
