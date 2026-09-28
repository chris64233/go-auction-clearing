package auctionclearing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// memRepository 是线程安全的聚合仓储：每场拍卖一把独立互斥锁，
// 保证针对同一场拍卖的“读-改-写”以及清算是原子且可串行化的；
// 不同拍卖之间互不阻塞。
type memRepository struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	data     map[string]*Aggregate
	onCommit func(agg *Aggregate) error // 可选的提交钩子（落盘）
}

// NewMemoryRepository 创建纯内存仓储（进程结束数据消失，多用于测试）。
func NewMemoryRepository() Repository {
	return &memRepository{locks: map[string]*sync.Mutex{}, data: map[string]*Aggregate{}}
}

// NewFileRepository 创建 JSON 文件持久化仓储：
// 每次提交以“临时文件 + rename”原子替换 dir/<拍卖ID>.json，
// 进程重启后数据自动加载；写入失败则整笔事务回滚。
func NewFileRepository(dir string) (Repository, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create repository dir: %w", err)
	}
	r := &memRepository{locks: map[string]*sync.Mutex{}, data: map[string]*Aggregate{}}
	r.onCommit = func(agg *Aggregate) error {
		return persistAggregate(dir, agg)
	}
	return r, nil
}

func (r *memRepository) keyLock(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[id]
	if !ok {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	return l
}

// CreateAuction 仅在 ID 不存在时写入，避免覆盖既有拍卖。
func (r *memRepository) CreateAuction(ctx context.Context, a *Auction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l := r.keyLock(a.ID)
	l.Lock()
	defer l.Unlock()
	if _, ok := r.data[a.ID]; ok {
		return NewError(KindConflict, "auction id already exists: "+a.ID)
	}
	agg := &Aggregate{Auction: *a, Bids: map[string]*Bid{}}
	if r.onCommit != nil {
		if err := r.onCommit(cloneAggregate(agg)); err != nil {
			return err
		}
	}
	r.data[a.ID] = agg
	return nil
}

// Update 在单场拍卖的事务内执行 fn。fn 拿到的是深拷贝，
// 只有返回 nil 时修改才会原子提交；fn 报错则整体放弃，不留中间状态。
func (r *memRepository) Update(ctx context.Context, auctionID string, fn func(agg *Aggregate) error) (*Aggregate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := r.keyLock(auctionID)
	l.Lock()
	defer l.Unlock()

	cur, ok := r.data[auctionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, auctionID)
	}
	work := cloneAggregate(cur)
	if err := fn(work); err != nil {
		return nil, err // 放弃全部修改，内存与磁盘均不变化
	}
	if r.onCommit != nil {
		// 先写盘成功再换内存，保证“提交”对两侧同时生效。
		if err := r.onCommit(cloneAggregate(work)); err != nil {
			return nil, err
		}
	}
	r.data[auctionID] = work
	return cloneAggregate(work), nil
}

// Load 返回聚合的只读深拷贝；读操作也取锁，确保不会读到半提交状态。
func (r *memRepository) Load(ctx context.Context, auctionID string) (*Aggregate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := r.keyLock(auctionID)
	l.Lock()
	defer l.Unlock()
	cur, ok := r.data[auctionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, auctionID)
	}
	return cloneAggregate(cur), nil
}

// cloneAggregate 深拷贝聚合，使调用方在锁外持有/修改副本互不影响。
func cloneAggregate(in *Aggregate) *Aggregate {
	out := &Aggregate{
		Auction: in.Auction,
		Bids:    make(map[string]*Bid, len(in.Bids)),
	}
	for k, b := range in.Bids {
		cp := *b
		if b.WithdrawnAt != nil {
			t := *b.WithdrawnAt
			cp.WithdrawnAt = &t
		}
		out.Bids[k] = &cp
	}
	if in.Clearing != nil {
		c := *in.Clearing
		// 用 make+copy 而非 append(nil, ...)，保证零成交时仍序列化为 [] 而非 null。
		c.Fills = make([]Fill, len(in.Clearing.Fills))
		copy(c.Fills, in.Clearing.Fills)
		out.Clearing = &c
	}
	return out
}

// persistAggregate 以临时文件 + rename 原子写入一场拍卖的 JSON 快照。
func persistAggregate(dir string, agg *Aggregate) error {
	data, err := json.MarshalIndent(agg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal aggregate: %w", err)
	}
	final := filepath.Join(dir, agg.Auction.ID+".json")
	tmp, err := os.CreateTemp(dir, ".tmp-"+agg.Auction.ID+"-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write aggregate: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("fsync aggregate: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		cleanup()
		return fmt.Errorf("rename aggregate: %w", err)
	}
	return nil
}

// LoadFileRepository 从目录恢复此前持久化的全部拍卖。
func LoadFileRepository(dir string) (Repository, error) {
	r, err := NewFileRepository(dir)
	if err != nil {
		return nil, err
	}
	store := r.(*memRepository)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read repository dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var agg Aggregate
		if err := json.Unmarshal(data, &agg); err != nil {
			return nil, fmt.Errorf("decode %s: %w", e.Name(), err)
		}
		if agg.Bids == nil {
			agg.Bids = map[string]*Bid{}
		}
		if _, exists := store.data[agg.Auction.ID]; exists {
			return nil, errors.New("duplicate auction id while loading: " + agg.Auction.ID)
		}
		store.data[agg.Auction.ID] = &agg
	}
	return r, nil
}
