package store

import (
	"os"
	"strconv"
)

// HEIMDB_TEST_LOG_CHUNK_BYTES を指定すると、コミットの記録を分けて書く大きさ（LogChunkBytes）をその値にしてテストを
// 流す（docs/plan.md の段階 59。小さくして、記録を読み書きするテスト全体で分けた記録を通す）。
func init() {
	if n, err := strconv.Atoi(os.Getenv("HEIMDB_TEST_LOG_CHUNK_BYTES")); err == nil && n > 0 {
		LogChunkBytes = n
	}
}
