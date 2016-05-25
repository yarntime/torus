package storage

import (
	"bytes"
	"fmt"
	"path/filepath"

	"runtime"
	"sync"

	"golang.org/x/net/context"

	"github.com/coreos/agro"
	"github.com/coreos/pkg/capnslog"
)

var _ agro.BlockStore = &mfileBlock{}

func init() {
	agro.RegisterBlockStore("mfile", newMFileBlockStore)
}

type mfileBlock struct {
	mut       sync.RWMutex
	dataFile  *MFile
	refFile   *MFile
	refIndex  map[agro.BlockRef]int
	closed    bool
	lastFree  int
	dfilename string
	mfilename string
	name      string
	blocksize uint64

	itPool sync.Pool
	// NB: Still room for improvement. Free lists, smart allocation, etc.
}

var blankRefBytes = make([]byte, agro.BlockRefByteSize)

func loadIndex(m *MFile) (map[agro.BlockRef]int, error) {
	clog.Infof("loading block index...")
	var membefore uint64
	if clog.LevelAt(capnslog.DEBUG) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		membefore = mem.Alloc
	}
	out := make(map[agro.BlockRef]int)
	for i := uint64(0); i < m.NumBlocks(); i++ {
		b := m.GetBlock(i)
		if bytes.Equal(blankRefBytes, b) {
			continue
		}
		out[agro.BlockRefFromBytes(b)] = int(i)
	}
	if clog.LevelAt(capnslog.DEBUG) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		clog.Debugf("index memory usage: %dK", ((mem.Alloc - membefore) / 1024))
	}
	clog.Infof("done loading block index")
	return out, nil
}

func newMFileBlockStore(name string, cfg agro.Config, meta agro.GlobalMetadata) (agro.BlockStore, error) {
	nBlocks := cfg.StorageSize / meta.BlockSize
	promBytesPerBlock.Set(float64(meta.BlockSize))
	promBlocksAvail.WithLabelValues(name).Set(float64(nBlocks))
	dpath := filepath.Join(cfg.DataDir, "block", fmt.Sprintf("data-%s.blk", name))
	mpath := filepath.Join(cfg.DataDir, "block", fmt.Sprintf("map-%s.blk", name))
	d, err := CreateOrOpenMFile(dpath, cfg.StorageSize, meta.BlockSize)
	if err != nil {
		return nil, err
	}
	m, err := CreateOrOpenMFile(mpath, nBlocks*agro.BlockRefByteSize, agro.BlockRefByteSize)
	if err != nil {
		return nil, err
	}
	refIndex, err := loadIndex(m)
	if err != nil {
		return nil, err
	}
	if m.NumBlocks() != d.NumBlocks() {
		panic("non-equal number of blocks between data and metadata")
	}
	promBlocks.WithLabelValues(name).Set(float64(len(refIndex)))
	return &mfileBlock{
		dataFile:  d,
		refFile:   m,
		refIndex:  refIndex,
		dfilename: dpath,
		mfilename: mpath,
		name:      name,
		blocksize: meta.BlockSize,
	}, nil
}

func (m *mfileBlock) Kind() string { return "mfile" }
func (m *mfileBlock) NumBlocks() uint64 {
	m.mut.RLock()
	defer m.mut.RUnlock()
	return m.numBlocks()
}

func (m *mfileBlock) BlockSize() uint64 {
	return m.blocksize
}

func (m *mfileBlock) numBlocks() uint64 {
	return m.dataFile.NumBlocks()
}

func (m *mfileBlock) UsedBlocks() uint64 {
	return uint64(len(m.refIndex))
}

func (m *mfileBlock) Flush() error {
	err := m.dataFile.Flush()

	if err != nil {
		return err
	}
	err = m.refFile.Flush()
	if err != nil {
		return err
	}
	promStorageFlushes.WithLabelValues(m.name).Inc()
	return nil
}

func (m *mfileBlock) Close() error {
	m.mut.Lock()
	defer m.mut.Unlock()
	return m.close()
}

func (m *mfileBlock) close() error {
	m.Flush()
	if m.closed {
		return nil
	}
	err := m.dataFile.Close()
	if err != nil {
		return err
	}
	err = m.refFile.Close()
	if err != nil {
		return err
	}
	m.closed = true
	return nil
}

func (m *mfileBlock) findIndex(s agro.BlockRef) int {
	if clog.LevelAt(capnslog.TRACE) {
		clog.Tracef("finding blockid %s", s)
	}
	if v, ok := m.refIndex[s]; ok {
		return v
	}
	return -1
}

func (m *mfileBlock) findEmpty() int {
	emptyBlock := make([]byte, agro.BlockRefByteSize)
	for i := uint64(0); i < m.numBlocks(); i++ {
		b := m.refFile.GetBlock((i + uint64(m.lastFree) + 1) % m.numBlocks())
		if bytes.Equal(b, emptyBlock) {
			m.lastFree = int((i + uint64(m.lastFree) + 1) % m.numBlocks())
			return m.lastFree
		}
	}
	return -1
}

func (m *mfileBlock) HasBlock(_ context.Context, s agro.BlockRef) (bool, error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	index := m.findIndex(s)
	if index == -1 {
		return false, nil
	}
	return true, nil
}

func (m *mfileBlock) GetBlock(_ context.Context, s agro.BlockRef) ([]byte, error) {
	m.mut.RLock()
	defer m.mut.RUnlock()
	if m.closed {
		promBlocksFailed.WithLabelValues(m.name).Inc()
		return nil, agro.ErrClosed
	}
	index := m.findIndex(s)
	if index == -1 {
		promBlocksFailed.WithLabelValues(m.name).Inc()
		return nil, agro.ErrBlockNotExist
	}
	clog.Tracef("mfile: getting block at index %d", index)
	promBlocksRetrieved.WithLabelValues(m.name).Inc()
	return m.dataFile.GetBlock(uint64(index)), nil
}

func (m *mfileBlock) WriteBlock(_ context.Context, s agro.BlockRef, data []byte) error {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return agro.ErrClosed
	}
	index := m.findEmpty()
	if index == -1 {
		clog.Error("mfile: out of space")
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return agro.ErrOutOfSpace
	}
	clog.Tracef("mfile: writing block at index %d", index)
	err := m.dataFile.WriteBlock(uint64(index), data)
	if err != nil {
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return err
	}
	err = m.refFile.WriteBlock(uint64(index), s.ToBytes())
	if err != nil {
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return err
	}
	if v := m.findIndex(s); v != -1 {
		// we already have it
		clog.Debug("mfile: block already exists", s)
		olddata := m.dataFile.GetBlock(uint64(v))
		if !bytes.Equal(olddata, data) {
			clog.Error("getting wrong data for block", s)
			clog.Errorf("%s, %s", olddata[:10], data[:10])
			return agro.ErrExists
		}
		// Not an error, if we already have it
		return nil
	}
	promBlocks.WithLabelValues(m.name).Inc()
	m.refIndex[s] = index
	promBlocksWritten.WithLabelValues(m.name).Inc()
	return nil
}

func (m *mfileBlock) WriteBuf(_ context.Context, s agro.BlockRef) ([]byte, error) {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return nil, agro.ErrClosed
	}
	index := m.findEmpty()
	if index == -1 {
		clog.Error("mfile: out of space")
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return nil, agro.ErrOutOfSpace
	}
	clog.Tracef("mfile: writing block at index %d", index)
	buf := m.dataFile.GetBlock(uint64(index))
	err := m.refFile.WriteBlock(uint64(index), s.ToBytes())
	if err != nil {
		promBlockWritesFailed.WithLabelValues(m.name).Inc()
		return nil, err
	}
	if v := m.findIndex(s); v != -1 {
		// we already have it
		clog.Debug("mfile: block already exists", s)
		// Not an error, if we already have it
		return nil, agro.ErrExists
	}
	promBlocks.WithLabelValues(m.name).Inc()
	m.refIndex[s] = index
	promBlocksWritten.WithLabelValues(m.name).Inc()
	return buf, nil
}

func (m *mfileBlock) DeleteBlock(_ context.Context, s agro.BlockRef) error {
	m.mut.Lock()
	defer m.mut.Unlock()
	if m.closed {
		promBlockDeletesFailed.WithLabelValues(m.name).Inc()
		return agro.ErrClosed
	}
	index := m.findIndex(s)
	if index == -1 {
		promBlockDeletesFailed.WithLabelValues(m.name).Inc()
		clog.Errorf("mfile: deleting non-existent thing? %s", s)
		return agro.ErrBlockNotExist
	}
	err := m.refFile.WriteBlock(uint64(index), blankRefBytes)
	if err != nil {
		promBlockDeletesFailed.WithLabelValues(m.name).Inc()
		return err
	}
	promBlocks.WithLabelValues(m.name).Dec()
	delete(m.refIndex, s)
	promBlocksDeleted.WithLabelValues(m.name).Inc()
	return nil
}

func (m *mfileBlock) BlockIterator() agro.BlockIterator {
	m.mut.RLock()
	defer m.mut.RUnlock()
	// TODO(barakmich): Amortize this alloc, eg, with Close() and a sync.Pool
	l := make([]agro.BlockRef, len(m.refIndex))
	i := 0
	for k := range m.refIndex {
		l[i] = k
		i++
	}
	return &mfileIterator{
		set: l,
		i:   -1,
	}
}

type mfileIterator struct {
	set  []agro.BlockRef
	i    int
	done bool
}

func (i *mfileIterator) Err() error { return nil }

func (i *mfileIterator) Next() bool {
	if i.done {
		return false
	}
	i.i++
	if i.i == len(i.set) {
		i.done = true
		return false
	}
	return true
}

func (i *mfileIterator) BlockRef() agro.BlockRef {
	return i.set[i.i]
}

func (i *mfileIterator) Close() error { return nil }
