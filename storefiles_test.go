package main

// Windows 的 t.TempDir 清理在文件仍被占用时直接失败，而 Linux 允许删除已打开
// 的文件。SQLite 驱动（close_v2 / 锁文件）可能在连接池归零之后再延迟一小段
// 时间才真正释放句柄，所以这里等待**真实状态**：把库文件改名再改回，成功即表示
// 操作系统已不再持有该文件。达到上界仍失败则把具体文件与错误交给测试报告，
// 而不是让 TempDir 抛出一句无从定位的 "file in use"。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const storeFileReleaseTimeout = 5 * time.Second

func firstLockedStoreFile(dirs []string) (string, error) {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return dir, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			probe := path + ".release-probe"
			if err := os.Rename(path, probe); err != nil {
				return path, err
			}
			if err := os.Rename(probe, path); err != nil {
				return probe, err
			}
		}
	}
	return "", nil
}

// waitForStoreFilesReleased blocks until every file in the given directories can
// be renamed, which is what TempDir cleanup needs on Windows.
func waitForStoreFilesReleased(t *testing.T, dirs ...string) {
	t.Helper()
	deadline := time.Now().Add(storeFileReleaseTimeout)
	for {
		locked, err := firstLockedStoreFile(dirs)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("store files still locked %s after shutdown: %s: %v", storeFileReleaseTimeout, locked, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// storeDirsOf collects the temporary directories that hold the given database
// paths so a test can wait for their release before TempDir cleanup runs.
func storeDirsOf(paths ...string) []string {
	dirs := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		dirs = append(dirs, filepath.Dir(path))
	}
	return dirs
}

var _ = strings.TrimSpace
