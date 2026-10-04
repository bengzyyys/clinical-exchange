package clinical

import (
	"strings"
	"sync"
	"testing"
)

// 压力验证：关闭与打开高频交错时，任何时刻成功持有目录的 Store 都必须是
// 唯一的——持有期间第二个 Open 必须立即被拒绝。
func TestLockExclusivityUnderChurn(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s, err := Open(dir)
				if err != nil {
					if !strings.Contains(err.Error(), "already in use") {
						t.Errorf("unexpected open error: %v", err)
					}
					continue
				}
				// 持有期间：第二个 Open 必须失败，否则独占已被破坏。
				if s2, err2 := Open(dir); err2 == nil {
					t.Error("second Open succeeded while directory was held")
					_ = s2.Close()
				}
				if err := s.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}
