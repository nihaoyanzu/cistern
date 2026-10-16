// Package reexec 提供进程自重启（syscall.Exec 替换当前镜像）。
package reexec

import (
	"fmt"
	"io"
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

// Resolve 解析当前进程对应的可执行路径（用于自更新落盘与重启）。
// 自更新若曾把旧文件改名为 .bak 再删除，/proc/self/exe 可能仍报告 .bak；
// 此时优先回退到去后缀的正式路径；若正式路径也不在磁盘上，则尝试从
// /proc/self/exe 把仍映射在内存中的镜像恢复到正式路径。
func Resolve() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位可执行文件失败: %w", err)
	}
	return resolvePath(exe)
}

// InstallPath 将 Resolve 结果规范为「应落盘/应 Exec」的正式路径（去掉 .bak/.new 与 (deleted)）。
func InstallPath(exe string) string {
	return canonicalExe(exe)
}

func resolvePath(exe string) (string, error) {
	exe = cleanReportedExe(exe)
	canon := canonicalExe(exe)

	// 候选：正式路径优先，再 EvalSymlinks / 原始报告路径
	candidates := make([]string, 0, 6)
	candidates = append(candidates, canon)
	if resolved, err := filepath.EvalSymlinks(canon); err == nil {
		candidates = append(candidates, resolved)
	}
	if exe != canon {
		candidates = append(candidates, exe)
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			candidates = append(candidates, resolved)
		}
	}
	if abs, err := filepath.Abs(cleanReportedExe(os.Args[0])); err == nil && abs != "" {
		candidates = append(candidates, canonicalExe(abs), abs)
	}

	seen := map[string]struct{}{}
	var firstExistingBak string
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		st, err := os.Stat(c)
		if err != nil || st.IsDir() {
			continue
		}
		// 绝不把仍叫 .bak 的路径当作正式安装目标（除非没有非 bak 可选）
		if strings.HasSuffix(c, ".bak") {
			if firstExistingBak == "" {
				firstExistingBak = c
			}
			continue
		}
		return c, nil
	}
	if firstExistingBak != "" {
		// 仅存在 .bak：若正式路径可写，尝试把 bak 视为可用镜像并复制到正式路径
		if restored := restoreToCanonical(firstExistingBak, canon); restored != "" {
			return restored, nil
		}
		return firstExistingBak, nil
	}

	// 磁盘上正式路径与报告路径都不在：进程可能仍占着已删 inode（Linux）
	if restored := recoverFromProcSelfExe(canon); restored != "" {
		return restored, nil
	}
	return "", fmt.Errorf("解析可执行路径失败: lstat %s: no such file or directory", canon)
}

func cleanReportedExe(exe string) string {
	exe = strings.TrimSpace(exe)
	// Go/Linux：已删文件时常带 " (deleted)" 后缀
	exe = strings.TrimSuffix(exe, " (deleted)")
	return strings.TrimSpace(exe)
}

func canonicalExe(exe string) string {
	exe = cleanReportedExe(exe)
	for {
		switch {
		case strings.HasSuffix(exe, ".bak"):
			exe = strings.TrimSuffix(exe, ".bak")
		case strings.HasSuffix(exe, ".new"):
			exe = strings.TrimSuffix(exe, ".new")
		default:
			return exe
		}
	}
}

func restoreToCanonical(src, canon string) string {
	if canon == "" || src == "" || src == canon {
		return ""
	}
	if st, err := os.Stat(canon); err == nil && !st.IsDir() {
		return canon
	}
	if err := copyFile(src, canon); err != nil {
		return ""
	}
	_ = os.Chmod(canon, 0o755)
	return canon
}

// recoverFromProcSelfExe 在 Linux 上从仍打开的 /proc/self/exe 恢复正式路径文件。
func recoverFromProcSelfExe(canon string) string {
	if canon == "" {
		return ""
	}
	if st, err := os.Stat(canon); err == nil && !st.IsDir() {
		return canon
	}
	src, err := os.Open("/proc/self/exe")
	if err != nil {
		return ""
	}
	defer src.Close()
	if err := writeReaderToFile(src, canon); err != nil {
		return ""
	}
	_ = os.Chmod(canon, 0o755)
	if st, err := os.Stat(canon); err == nil && !st.IsDir() {
		return canon
	}
	return ""
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeReaderToFile(in, dst)
}

func writeReaderToFile(in io.Reader, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := dst + ".reexec-restore"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
