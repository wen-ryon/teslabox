package teslacam

import (
	"bytes"
	"io"
	"os"
)

type durationRead struct {
	offset int64
	data   []byte
}
type durationProof struct {
	size  int64
	reads []durationRead
}

// proofReader 只记录决定时长的读取证据；不缓存录像载荷，单文件证据限制为 4 KiB。
type proofReader struct {
	source   io.ReaderAt
	proof    durationProof
	used     int
	overflow bool
}

func (r *proofReader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.source.ReadAt(p, offset)
	if err != nil {
		r.overflow = true
		r.proof.reads = nil
	}
	if err == nil && !r.overflow {
		if r.used+n > 4096 || len(r.proof.reads) >= 128 {
			r.overflow = true
			r.proof.reads = nil
		} else {
			r.used += n
			r.proof.reads = append(r.proof.reads, durationRead{offset: offset, data: bytes.Clone(p[:n])})
		}
	}
	return n, err
}

// matches 在新快照内核对全部解析字段，同大小同修改时间的覆盖也不能绕过内容校验。
func (p *durationProof) matches(path string) bool {
	if p == nil || len(p.reads) == 0 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != p.size {
		return false
	}
	reader := &durationPageReader{source: file, size: p.size, pages: make(map[int64][]byte)}
	var scratch [32]byte
	for _, read := range p.reads {
		if len(read.data) > len(scratch) {
			return false
		}
		buf := scratch[:len(read.data)]
		if _, err := reader.ReadAt(buf, read.offset); err != nil || !bytes.Equal(buf, read.data) {
			return false
		}
	}
	return true
}

// durationPageReader 合并临近字段读取，临时页缓存限制为 64 KiB，完成一个文件后释放。
type durationPageReader struct {
	source io.ReaderAt
	size   int64
	pages  map[int64][]byte
}

func (r *durationPageReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 || offset+int64(len(p)) > r.size {
		return 0, io.EOF
	}
	base := offset / 4096 * 4096
	if offset+int64(len(p)) > base+4096 {
		return r.source.ReadAt(p, offset)
	}
	page, ok := r.pages[base]
	if !ok {
		if len(r.pages) >= 16 {
			return r.source.ReadAt(p, offset)
		}
		page = make([]byte, min(int64(4096), r.size-base))
		if _, err := r.source.ReadAt(page, base); err != nil {
			return 0, err
		}
		r.pages[base] = page
	}
	copy(p, page[offset-base:])
	return len(p), nil
}
