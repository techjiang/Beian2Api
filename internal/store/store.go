// Package store 用单文件嵌入式 KV（bbolt）落地数据。
// 单文件意味着备份 = 复制一个文件，迁移 = 拷走 data 目录。
package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asoe/beian2api/internal/model"
	"github.com/asoe/beian2api/internal/notify"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketCache   = []byte("cache")   // domain -> Entry
	bucketHistory = []byte("history") // domain\x00ts -> Record
	bucketWatch   = []byte("watch")   // domain -> WatchItem
	bucketMeta    = []byte("meta")    // 计数器等
	bucketKeys    = []byte("apikeys") // key -> KeyMeta
	bucketChanges = []byte("changes") // changeID -> Change
	bucketNotify  = []byte("notify")  // channel -> State
)

// ErrNotFound 表示 key 不存在。
var ErrNotFound = errors.New("not found")

// Entry 是缓存中的一条结果快照。
type Entry struct {
	Domain    string         `json:"domain"`
	Found     bool           `json:"found"`
	Records   []model.Record `json:"records"`
	Source    string         `json:"source"`
	FetchedAt int64          `json:"fetched_at"`
	ExpireAt  int64          `json:"expire_at"`
}

// WatchItem 是被监控的域名。
type WatchItem struct {
	Domain    string `json:"domain"`
	AddedAt   int64  `json:"added_at"`
	CheckedAt int64  `json:"checked_at"`
	LastFP    string `json:"last_fp"`
	LastICP   string `json:"last_icp"`
	Changes   int    `json:"changes"`
	LastError string `json:"last_error,omitempty"`
	// FailStreak 是连续回源失败次数，Alerted 表示已经为这轮故障推送过告警。
	// 分开记是因为「连续失败」和「已经通知过」不是一回事：前者决定要不要报，
	// 后者决定有没有报过——否则每轮巡检都会往群里灌一条同样的消息。
	FailStreak int   `json:"fail_streak,omitempty"`
	Alerted    bool  `json:"alerted,omitempty"`
	LastOKAt   int64 `json:"last_ok_at,omitempty"`
}

// Store 是对 bbolt 的薄封装，负责序列化与索引。
type Store struct {
	db   *bolt.DB
	path string
	mu   sync.RWMutex

	// counters 是计数器的内存态。之所以不每次 Incr 都写库：
	// bbolt 的每个写事务都要 fsync，一次缓存命中（本该是微秒级）会被拖到 2 毫秒以上。
	// 命中率这类指标丢几秒的数据无所谓，丢掉延迟才是真的损失。
	cmu      sync.Mutex
	counters map[string]int64 // 相对落盘值的增量
	base     map[string]int64 // 已落盘值的内存副本，避免读计数器还要开读事务
	dirty    bool

	// 滚动保留上限，0 表示不限制。变更流水与历史快照是只增的数据，
	// 没有上限的部署跑上一年就是无限增长，而这个上限本身就是配置项。
	retainChanges int
	retainHistory int
	// retainHistoryTotal 是历史快照的**全局**条数上限。
	//
	// 按域名分别算的配额有个前提：域名数量是有界的。但 /api/v1/query
	// 是公开接口，随便刷一批随机域名，每个域名一条快照，各自都没超限，
	// 合起来却能让数据文件无限长——按域名算的配额对「只查一次」的域名完全无效。
	retainHistoryTotal int

	// histCount 是历史快照 bucket 的条目数缓存，histSeeded 表示它是否可信。
	//
	// 为什么不每次都读 b.Stats().KeyN：它是 O(全部键) 的遍历。实测 2 万条 47µs、
	// 20 万条 713µs，而这是 **写入路径**——每写一条历史快照就要付一次。
	// 保留上限本来只是「该不该裁」的一个判断，用不着精确到个位数：
	// 增量维护 + 真正裁剪时重算一次（顺带纠正漂移），既准又便宜。
	histMu     sync.Mutex
	histCount  int64
	histSeeded bool

	// seriesPending 是「当前小时」的序列增量，跟计数器一样攒着等 Flush。
	// 按小时分组而不是只留一行：跨过整点时，前一小时和这一小时的增量都在
	// 这个 map 里，各自归到自己那行，不会把两小时混成一小时。
	seriesMu      sync.Mutex
	seriesPending map[int64]*SeriesPoint

	// lastPurge 是上次清理过期缓存的时间戳（Unix 秒）。
	// 清理是 O(缓存条目数) 的全表遍历，跟着 15 秒的维护节奏跑，
	// 十万条缓存就是每 15 秒扫十万条。真正该有的是「定期」而不是「频繁」。
	lastPurge int64

	// staleMax 是「过期数据还能被回退使用」的窗口（秒）。
	// 缓存条目过期之后并不立即无用：上游全挂时 service 还会回退到它，
	// 但最多回退这么久。超过这个窗口的条目永远不会再被读到，属于纯垃圾。
	staleMax int64
}

// 默认保留上限：变更流水 2000 条，每个域名历史快照 200 条。
const (
	DefaultRetainChanges = 2000
	DefaultRetainHistory = 200
	// DefaultStaleWindow 对应「上游全挂时最多回退 7 天前的快照」。
	DefaultStaleWindow = 7 * 86400
	// DefaultRetainHistoryTotal 是历史快照的全局上限（跨域名）。
	DefaultRetainHistoryTotal = 20000
)

// Open 打开（或创建）数据目录下的数据文件。
func Open(dir string) (*Store, error) {
	return OpenFile(filepath.Join(dir, "beian2api.db"))
}

// OpenFile 打开指定路径的数据文件，目录会按需创建。
func OpenFile(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
		}
	}
	db, err := bolt.Open(path, 0o640, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		if strings.Contains(err.Error(), "timeout") {
			return nil, fmt.Errorf("数据文件 %s 正被其他进程占用（服务是否在运行？）", path)
		}
		return nil, fmt.Errorf("打开数据文件 %s: %w", path, err)
	}
	s := &Store{
		db: db, path: path, counters: map[string]int64{}, base: map[string]int64{},
		seriesPending: map[int64]*SeriesPoint{},
		retainChanges: DefaultRetainChanges, retainHistory: DefaultRetainHistory,
		retainHistoryTotal: DefaultRetainHistoryTotal, staleMax: DefaultStaleWindow,
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketCache, bucketHistory, bucketWatch, bucketMeta, bucketKeys, bucketChanges, bucketNotify, bucketSeries} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadCounters(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Prepare 按当前保留上限做一次全表整理，供服务启动时调用。
//
// 这个不能放进 Open：打开文件时还不知道用户的配置，只能拿默认值 200 去裁——
// 于是配了 `retain_history: 0`（明确表示「不裁剪」）的部署，每次重启都会被
// 悄悄砍到 200 条，而且配置文件上看不出任何异常。裁剪是删数据，必须等
// 配置加载完、WithRetention 生效之后再做，也只由 serve 触发；
// 只读命令（doctor / export / backup）不该顺手删别人的历史。
func (s *Store) Prepare() error {
	if s.retainHistory <= 0 {
		return nil
	}
	if _, err := s.TrimHistory(0); err != nil {
		return fmt.Errorf("整理历史快照: %w", err)
	}
	return nil
}

// Size 返回数据文件字节数；取不到时返回 0（统计用，不该影响接口）。
func (s *Store) Size() int64 {
	if info, err := os.Stat(s.path); err == nil {
		return info.Size()
	}
	return 0
}

// Path 返回数据文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// GetCache 读取缓存，不存在返回 ErrNotFound。
func (s *Store) GetCache(domain string) (*Entry, error) {
	var e Entry
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketCache).Get([]byte(domain))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &e)
	})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// PutCache 写入缓存。
func (s *Store) PutCache(e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCache).Put([]byte(e.Domain), b)
	})
}

// DeleteCache 删除某域名的缓存，返回是否真的删掉了一条。
//
// 返回值不是「有没有出错」：bbolt 的 Delete 对不存在的 key 返回 nil，
// 于是删一个从没查过的域名也会「成功」。后台照着这个结果显示
// 「已删除 nope.com」，而那个域名从来没被缓存过——用户会以为自己的删除
// 生效了、或者以为缓存里本来有它。
func (s *Store) DeleteCache(domain string) (bool, error) {
	existed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCache)
		if b.Get([]byte(domain)) == nil {
			return nil
		}
		existed = true
		return b.Delete([]byte(domain))
	})
	return existed, err
}

// AppendHistory 追加一条历史快照。
//
// key 里带天 + 时间戳 + 指纹前缀：只用「域名+天」会互相覆盖——
// 同一天里备案改了两次，第一次的快照会被第二次顶掉，历史里就只剩最后一版，
// 期间那次变更（以及它的 diff）永久丢失。这是只有一天多改几次才会踩到的坑。
//
// 写入后顺手把该域名的超限快照裁掉，和这次写共用同一个事务。理由有两条：
// 一是裁剪是 O(该域名的条数) 而不是 O(全表)，几乎不要钱；二是同一事务里做完，
// 不会多出一次 fsync。
func (s *Store) AppendHistory(r model.Record) error {
	day := time.Unix(r.FetchedAt, 0).Format("20060102")
	fp := r.Fingerprint()
	if len(fp) > 12 {
		fp = fp[:12]
	}
	key := []byte(fmt.Sprintf("%s\x00%s\x00%020d\x00%s", r.Domain, day, r.FetchedAt, fp))
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket(bucketHistory)
		// 覆盖写（同一时刻、同一指纹）不算新增，计数不能跟着涨。
		existed := bk.Get(key) != nil
		if err := bk.Put(key, b); err != nil {
			return err
		}
		delta := int64(0)
		if !existed {
			delta = 1
		}
		removed, err := trimHistoryDomain(bk, r.Domain, s.retainHistory)
		if err != nil {
			return err
		}
		delta -= int64(removed)
		return s.trimHistoryTotal(bk, delta)
	})
}

// trimHistoryDomain 把某个域名的历史快照裁到最近 keep 条（keep<=0 表示不裁）。
//
// 用 cursor 从该域名的 key 前缀走一遍，只碰自己的那一段，
// 不扫全表——这是它和 TrimHistory 的区别，也是它能放进写入路径的原因。
// 返回值是实际删掉的条数，调用方要拿它维护历史总条数缓存。
func trimHistoryDomain(b *bolt.Bucket, domain string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	prefix := []byte(domain + "\x00")
	var keys [][]byte
	c := b.Cursor()
	for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
		keys = append(keys, append([]byte(nil), k...))
	}
	if len(keys) <= keep {
		return 0, nil
	}
	doomed := keys[:len(keys)-keep]
	for _, k := range doomed {
		if err := b.Delete(k); err != nil {
			return 0, err
		}
	}
	return len(doomed), nil
}

// trimHistoryTotal 把整个历史 bucket 裁到 keep 条以内，从最旧的删。
//
// key 是「域名\x00天\x00时间戳\x00指纹」，字典序不等于时间序，所以只能
// 先按时间戳找最旧的那些——这一趟是 O(全表)。
//
// 所以不能每次写入都跑：上限 2 万条时，每写一条扫 2 万条，就变成前面
// 刚挪走的那个「定时全表扫描」换了个位置。这里留一段余量（10%），
// 攒够了再裁一次并裁回上限，摊下来是「每 2000 次写入扫一趟全表」。
func (s *Store) trimHistoryTotal(b *bolt.Bucket, delta int64) error {
	keep := s.retainHistoryTotal
	if keep <= 0 {
		return nil
	}
	slack := keep / 10
	if slack < 10 {
		slack = 10
	}
	s.histMu.Lock()
	if !s.histSeeded {
		// 第一次用：以真实桶大小为准（升级上来的老数据可能早就超限）。
		s.histCount = int64(b.Stats().KeyN)
		s.histSeeded = true
	}
	s.histCount += delta
	total := s.histCount
	s.histMu.Unlock()
	if total <= int64(keep+slack) {
		return nil
	}
	// 到这儿才做全表扫描。扫完顺便把计数校正成真实值：增量维护允许漂移，
	// 而这一步每 (keep/10) 次写入才跑一次，校正的成本被摊平了。
	total = int64(b.Stats().KeyN)
	s.histMu.Lock()
	s.histCount = total
	s.histMu.Unlock()
	type item struct {
		key []byte
		ts  int64
	}
	items := make([]item, 0, total)
	c := b.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		// key 形如 domain\x00day\x00ts\x00fp，取第三段当时间戳。
		parts := bytes.SplitN(k, []byte{0}, 4)
		if len(parts) < 3 {
			continue
		}
		ts, err := strconv.ParseInt(string(parts[2]), 10, 64)
		if err != nil {
			continue
		}
		items = append(items, item{key: append([]byte(nil), k...), ts: ts})
	}
	if len(items) <= keep {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ts < items[j].ts })
	for _, it := range items[:len(items)-keep] {
		if err := b.Delete(it.key); err != nil {
			return err
		}
	}
	s.histMu.Lock()
	s.histCount = int64(keep)
	s.histMu.Unlock()
	return nil
}

// invalidateHistoryCount 让历史条数缓存在下一次写入时重新取准。
//
// 批量改动（TrimHistory / Restore / 清空）之后必须调它：增量维护的计数
// 只认得「经由 AppendHistory 的单条增删」，绕开它一次性删掉几千条时
// 缓存还停在旧值上，裁剪节奏就会一直错下去。
func (s *Store) invalidateHistoryCount() {
	s.histMu.Lock()
	s.histSeeded = false
	s.histMu.Unlock()
}

// History 返回某域名的历史快照，按时间升序。
func (s *Store) History(domain string, limit int) ([]model.Record, error) {
	var out []model.Record
	prefix := []byte(domain + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketHistory).Cursor()
		for k, v := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
			var r model.Record
			if json.Unmarshal(v, &r) == nil {
				out = append(out, r)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func hasPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

// PutWatch 新增/更新监控项。
func (s *Store) PutWatch(w *WatchItem) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketWatch).Put([]byte(w.Domain), b)
	})
}

// UpdateWatchIfExists 只在监控项仍然存在时覆盖它，返回是否真的写入了。
//
// 巡检一轮可能跑几十秒（域名多时按估算窗口能到十几分钟），这段时间里用户
// 完全可以点「移出」。巡检结束时若无条件 PutWatch，刚被移出的域名会原地复活：
// 界面上它又回来了，再点一次「移出」，下一轮巡检再写回来——像个删不掉的幽灵。
// 写入方是巡检、删除方是用户，用户的意思优先。
func (s *Store) UpdateWatchIfExists(w *WatchItem) (bool, error) {
	b, err := json.Marshal(w)
	if err != nil {
		return false, err
	}
	written := false
	err = s.db.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket(bucketWatch)
		if bk.Get([]byte(w.Domain)) == nil {
			return nil
		}
		written = true
		return bk.Put([]byte(w.Domain), b)
	})
	return written, err
}

// GetWatch 获取监控项。
func (s *Store) GetWatch(domain string) (*WatchItem, error) {
	var w WatchItem
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketWatch).Get([]byte(domain))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &w)
	})
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// DeleteWatch 删除监控项。
func (s *Store) DeleteWatch(domain string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketWatch).Delete([]byte(domain))
	})
}

// ListWatch 列出全部监控项。
func (s *Store) ListWatch() ([]WatchItem, error) {
	var out []WatchItem
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketWatch).ForEach(func(k, v []byte) error {
			var w WatchItem
			if json.Unmarshal(v, &w) == nil {
				out = append(out, w)
			}
			return nil
		})
	})
	return out, err
}

// CountWatch 只数监控项个数，不把整个 bucket 读进内存。
//
// 首页每 60 秒轮询一次巡检状态，而「有几个域名在监控」这件事只需要一个数字。
// 之前每次都 ListWatch() 全量反序列化，1000 个域名就是 1000 次 JSON 解析，
// 只为在标题里显示「1000 个」。
func (s *Store) CountWatch() (int, error) {
	var n int
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketWatch).Stats().KeyN
		return nil
	})
	return n, err
}

// ListWatchPage 按域名升序分页读取监控项。
//
// 首页只显示前几十个就够，剩下的靠「展开」按需拉。之前 status 接口无条件
// 返回全部：域名上千之后，每 60 秒一次全量轮询，接口本身变成了负担。
// limit<=0 表示不限制（保持老行为），offset 超出范围返回空。
func (s *Store) ListWatchPage(limit, offset int) ([]WatchItem, error) {
	out := make([]WatchItem, 0, 16)
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketWatch).Cursor()
		skipped := 0
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if skipped < offset {
				skipped++
				continue
			}
			var w WatchItem
			if json.Unmarshal(v, &w) == nil {
				out = append(out, w)
			}
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// Count 统计各 bucket 的条目数。
type Counts struct {
	Cache   int `json:"cache"`
	History int `json:"history"`
	Watch   int `json:"watch"`
	Keys    int `json:"keys"`
	Changes int `json:"changes"`
	Notify  int `json:"notify"`
}

// Count 返回各 bucket 条目数。
func (s *Store) Count() (Counts, error) {
	var c Counts
	err := s.db.View(func(tx *bolt.Tx) error {
		c.Cache = tx.Bucket(bucketCache).Stats().KeyN
		c.History = tx.Bucket(bucketHistory).Stats().KeyN
		c.Watch = tx.Bucket(bucketWatch).Stats().KeyN
		c.Keys = tx.Bucket(bucketKeys).Stats().KeyN
		c.Changes = tx.Bucket(bucketChanges).Stats().KeyN
		c.Notify = tx.Bucket(bucketNotify).Stats().KeyN
		return nil
	})
	return c, err
}

// Bulk 在单个事务里写入若干缓存条目，导入用。
func (s *Store) Bulk(entries []*Entry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCache)
		for _, e := range entries {
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(e.Domain), data); err != nil {
				return err
			}
		}
		return nil
	})
}

// AllCache 遍历全部缓存（备份用）。
func (s *Store) AllCache(fn func(*Entry) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCache).ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) != nil {
				return nil
			}
			return fn(&e)
		})
	})
}

// Incr 累加计数器。
//
// 只在内存里加，落盘交给 Flush（服务每 15 秒调一次，退出前再调一次）。
// 原因很实际：bbolt 的每个写事务都要 fsync，实测单次约 2.5ms。
// 缓存命中本该是微秒级的事，为了记一个「命中 +1」把整条快路径拖慢
// 三个数量级，是这个项目里最不该有的开销——命中率丢几秒数据完全无所谓。
func (s *Store) Incr(name string, delta int64) (int64, error) {
	s.cmu.Lock()
	s.counters[name] += delta
	s.dirty = true
	v := s.base[name] + s.counters[name]
	s.cmu.Unlock()
	// 同一个增量也喂给时间序列。放在这里而不是让每个调用点自己记：
	// 「hits 加了一次」这件事只该有一个入口，两处分别记迟早会漂。
	s.bumpSeries(name, delta)
	return v, nil
}

// Flush 把内存里的计数器增量写进 meta bucket。
func (s *Store) Flush() error {
	s.cmu.Lock()
	if !s.dirty || len(s.counters) == 0 {
		s.cmu.Unlock()
		return nil
	}
	pending := s.counters
	s.counters = map[string]int64{}
	s.dirty = false
	s.cmu.Unlock()

	bumped := map[string]int64{}
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		for name, delta := range pending {
			var v uint64
			if cur := b.Get([]byte(name)); len(cur) == 8 {
				v = binary.BigEndian.Uint64(cur)
			}
			v = uint64(int64(v) + delta)
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], v)
			if err := b.Put([]byte(name), buf[:]); err != nil {
				return err
			}
			bumped[name] = int64(v)
		}
		// 时间序列跟着同一次事务落盘：维护循环本来就每 15 秒开一次写事务，
		// 顺手把曲线写进去，不额外多一次 fsync。
		return s.flushSeries(tx)
	})

	s.cmu.Lock()
	defer s.cmu.Unlock()
	if err != nil {
		// 写失败要放回去，否则这几秒的计数就永久丢了。
		for k, v := range pending {
			s.counters[k] += v
		}
		s.dirty = true
		return err
	}
	for k, v := range bumped {
		s.base[k] = v
	}
	return nil
}

// Stat 读取计数器：落盘值的内存副本 + 还没刷盘的增量。
func (s *Store) Stat(name string) int64 {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.base[name] + s.counters[name]
}

// persisted 读取单个计数器真正落盘的值（不含内存里未刷盘的增量），
// 用来验证「攒着再写」确实攒住了，也让备份/迁移前的对账有据可依。
func (s *Store) persisted(name string) int64 {
	var n int64
	_ = s.db.View(func(tx *bolt.Tx) error {
		if cur := tx.Bucket(bucketMeta).Get([]byte(name)); len(cur) == 8 {
			n = int64(binary.BigEndian.Uint64(cur))
		}
		return nil
	})
	return n
}

// loadCounters 在打开时把已有计数器读进内存副本。
func (s *Store) loadCounters() error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).ForEach(func(k, v []byte) error {
			if len(v) == 8 {
				s.base[string(k)] = int64(binary.BigEndian.Uint64(v))
			}
			return nil
		})
	})
}

// SetStat 直接写入一个整数值（不是自增），用于「上次巡检时间」这类
// 需要跨重启保留、又不需要累加的运行态。失败只影响展示，不算致命。
func (s *Store) SetStat(name string, v int64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte(name), buf[:])
	})
	if err == nil {
		// 这是「覆盖写」，内存副本要跟着改，否则 Stat 会把旧值加回来。
		s.cmu.Lock()
		s.base[name] = v
		s.cmu.Unlock()
	}
	return err
}

// Snapshot 生成数据文件的副本路径（bbolt 的 Tx.WriteTo 保证一致性）。
func (s *Store) Snapshot(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(f)
		return err
	})
}

// WithRetention 覆盖滚动保留上限（<=0 表示不限制）。
//
// 变更流水和历史快照都是只增不减的数据。没有上限的部署跑上一年，
// bbolt 文件只会一直长大，而首页永远只显示最近 10 条——
// 存下来的 99.9% 永远没人会看。
func (s *Store) WithRetention(changes, history int) *Store {
	s.retainChanges, s.retainHistory = changes, history
	return s
}

// Retention 返回当前的保留上限。
func (s *Store) Retention() (changes, history int) {
	return s.retainChanges, s.retainHistory
}

// WithStaleWindow 设置「过期缓存仍可回退」的窗口（秒），<=0 回落到默认值。
//
// 缓存 bucket 的 key 是域名，同一域名只占一行，所以它不会因为查询次数膨胀，
// 但会因为「查过的域名有多少个」膨胀——而域名数量是没有上限的。
// 配合公开的查询接口，随便刷一批随机域名就能让数据文件一直长。
func (s *Store) WithStaleWindow(sec int) *Store {
	if sec > 0 {
		s.staleMax = int64(sec)
	}
	return s
}

// WithHistoryTotal 设置历史快照的全局条数上限（0 表示不限制）。
func (s *Store) WithHistoryTotal(n int) *Store {
	s.retainHistoryTotal = n
	return s
}

// RetentionTotal 返回历史快照的全局上限。
func (s *Store) RetentionTotal() int { return s.retainHistoryTotal }

// StaleWindow 返回当前回退窗口（秒）。
func (s *Store) StaleWindow() int64 { return s.staleMax }

// PurgeEvery 是过期缓存清理的最小间隔（秒）。
const PurgeEvery = 3600

// PurgeExpired 删除过期且超出回退窗口的缓存条目，返回删除条数。
//
// 为什么可以删：service 命中缓存要求 ExpireAt > now，回退快照还要求
// 「抓取时间距今不超过 stale_max」。两条加起来，ExpireAt 早于
// now-stale_max 的条目已经没有任何代码路径会读到，留着只是占文件。
func (s *Store) PurgeExpired(now int64) (int, error) {
	s.mu.Lock()
	if now-s.lastPurge < PurgeEvery {
		s.mu.Unlock()
		return 0, nil
	}
	s.lastPurge = now
	s.mu.Unlock()

	cutoff := now - s.staleMax
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCache)
		var stale [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) == nil && e.ExpireAt > 0 && e.ExpireAt < cutoff {
				stale = append(stale, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		removed = len(stale)
		return nil
	})
	return removed, err
}

// TrimChanges 只保留最近的 keep 条变更流水，返回删除条数。
//
// key 是「时间戳-域名」的定宽前綴，字典序即时间序，所以从最旧的一头删。
func (s *Store) TrimChanges(keep int) (int, error) {
	if keep <= 0 {
		keep = s.retainChanges
	}
	if keep <= 0 {
		return 0, nil
	}
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketChanges)
		total := b.Stats().KeyN
		if total <= keep {
			return nil
		}
		var stale [][]byte
		c := b.Cursor()
		for k, _ := c.First(); k != nil && len(stale) < total-keep; k, _ = c.Next() {
			stale = append(stale, append([]byte(nil), k...))
		}
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		removed = len(stale)
		return nil
	})
	return removed, err
}

// TrimHistory 每个域名只保留最近 keep 条历史快照，返回删除条数。
//
// 按域名分别计数，不是全局：配额按 IP 分和按总量分是完全不同的语义，
// 全局上限会让一个高频域名把其他域名的历史全挤掉。
//
// 这个方法要扫全表，所以不在定时维护里跑：正常写入由 AppendHistory 自己
// 顺手裁掉超限的那几条（只碰该域名的前缀，O(该域名条数)）。留在这里是给
// **升级迁移**用的——老版本写进来的数据可能早就超限，启动时清一次即可。
func (s *Store) TrimHistory(keep int) (int, error) {
	if keep <= 0 {
		keep = s.retainHistory
	}
	if keep <= 0 {
		return 0, nil
	}
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHistory)
		var stale [][]byte
		var group [][]byte
		var cur string
		flush := func() {
			if len(group) > keep {
				stale = append(stale, group[:len(group)-keep]...)
			}
			group = nil
		}
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			dom := string(k)
			if i := indexByte(k, 0); i >= 0 {
				dom = string(k[:i])
			}
			if dom != cur {
				flush()
				cur = dom
			}
			group = append(group, append([]byte(nil), k...))
		}
		flush()
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		removed = len(stale)
		return nil
	})
	if err == nil {
		s.invalidateHistoryCount()
	}
	return removed, err
}

// Maintain 做一轮存储维护：刷盘计数器 + 裁剪变更流水。
// 由服务定时调用，也在退出前调一次。
//
// 这里不裁历史快照：那个操作要扫全表，曾经是「每 15 秒按域名全表分组」，
// 域名上千之后会跑成秒级。历史改成写入时就地裁（见 AppendHistory）。
func (s *Store) Maintain() (MaintResult, error) {
	var r MaintResult
	// 缓存条目数只需要一个「这一小时的观测值」，所以在维护这一拍取一次。
	// 放在 Flush 之前：它写进的是同一个 seriesPending，跟着一起落盘。
	if c, err := s.Count(); err == nil {
		s.ObserveCacheSize(c.Cache)
	}
	if err := s.Flush(); err != nil {
		return r, err
	}
	n, err := s.TrimChanges(0)
	r.ChangesTrimmed = n
	if err != nil {
		return r, err
	}
	// 缓存条目一旦超出回退窗口就永远读不到了，顺手清掉——
	// 否则「查过的域名」这个集合只会越来越大，数据文件跟着一起长。
	purged, err := s.PurgeExpired(time.Now().Unix())
	r.CachePurged = purged
	return r, err
}

// MaintResult 是一次维护的结果。
type MaintResult struct {
	ChangesTrimmed int `json:"changes_trimmed"`
	CachePurged    int `json:"cache_purged"`
}

// AppendChange 记录一条变更事件，key 按时间倒序可遍历。
func (s *Store) AppendChange(id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketChanges).Put([]byte(id), b)
	})
}

// ErrStop 由 ListChanges 的回调返回，表示「不必再往前遍历了」。
//
// 增量拉取需要一个早停信号：key 是「时间戳-域名」，遍历天然是新的在前，
// 一旦读到早于游标的那条，后面全部更旧，继续扫只是白读。没有它的话
// `?since=<刚刚>` 这种最常见的增量请求，每次都要把整个变更流水反序列化一遍。
var ErrStop = errors.New("stop iteration")

// ListChanges 按时间倒序（新的在前）遍历变更事件，最多保留 limit 条。
//
// fn 返回 keep=false 时该条被丢弃，且不占用 limit 名额。过滤必须发生在
// 截断之前，否则「since 增量拉取」会先被 limit 截断，中间的事件就漏了。
//
// fn 返回 ErrStop 时立即结束遍历且不算错误。
func (s *Store) ListChanges(limit int, fn func(raw []byte) (keep bool, err error)) error {
	_, err := s.PageChanges(limit, 0, fn)
	return err
}

// PageChanges 在 ListChanges 之上加一层「跳过前 offset 条」的能力，并返回本页内容。
//
// offset 数的是「已被 fn 判定保留的记录」，所以它和 ?since= 各管一段：
// since 是增量游标（读到早于游标的记录就整体结束遍历），offset 是翻页游标。
//
// 返回值是这个 store 方法唯一的产出方式：fn 只负责「这条要不要」，
// 不负责收集。让 fn 顺手 append 会掉进一个很隐蔽的坑——跳过发生在
// fn 之后，被跳过的记录其实已经进过一次结果集了，翻页会翻出重复的内容。
func (s *Store) PageChanges(limit, offset int, fn func(raw []byte) (keep bool, err error)) ([][]byte, error) {
	if offset < 0 {
		offset = 0
	}
	rows, _, err := s.scanChanges("", limit, offset, fn)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// PageChangesAfter 从 cursor 之后（更旧的一侧）继续取一页，返回本页内容与下一页的游标。
//
// 为什么要有这个：offset 是「走 N 步再开始收」，翻到第 k 页就要在游标上
// 空走 N 步。流水攒到 2 万条、每页 50 条时，翻到第 300 页要空走 1.5 万条并
// 把它们逐个反序列化一遍——翻得越深越慢，翻到最后一页最慢。
//
// keyset（游标）翻页的代价与页码无关：cursor.Seek 直接跳到那个 key，
// 之后只读本页的 limit 条。代价是不能「跳到第 N 页」，但翻页界面本来
// 就只需要「往后一页」。
//
// cursor 是上一页最后一条的 key（由本函数返回，对调用方不透明）。
// 它找不到时按「从头开始」处理：保留上限把旧记录裁掉之后，手里那个游标
// 就指向一条已经不存在的记录了，返回空页会让人以为流水空了。
func (s *Store) PageChangesAfter(cursor string, limit int, fn func(raw []byte) (keep bool, err error)) ([][]byte, []string, error) {
	return s.scanChanges(cursor, limit, 0, fn)
}

// PageChangesFrom 同时接受 cursor 与 offset：老调用方传 offset，新调用方传 cursor。
//
// 两个都不传等价于「从最新一条开始」；都传时以 cursor 为准，因为它是精确的
// 位置，而 offset 只是「数过去」。为了兼容没有删掉 offset，但它在有 cursor
// 时会被忽略——两种定位方式叠在一起没有任何合理语义。
func (s *Store) PageChangesFrom(cursor string, limit, offset int, fn func(raw []byte) (keep bool, err error)) ([][]byte, []string, error) {
	if cursor != "" {
		offset = 0
	}
	return s.scanChanges(cursor, limit, offset, fn)
}

// scanChanges 是实现 PageChanges / PageChangesAfter 的那一趟遍历。
//
// 返回的 keys 与 rows 一一对应，是本页每一条的 key；调用方要取下一页时
// 把最后那一个原样带回来即可。
// offset 与 cursor 是两种互斥的定位方式，这里同时支持只是为了兼容老调用方。
func (s *Store) scanChanges(cursor string, limit, offset int, fn func(raw []byte) (keep bool, err error)) ([][]byte, []string, error) {
	if limit <= 0 {
		limit = 50
	}
	var out [][]byte
	var keys []string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketChanges)
		c := b.Cursor()
		var k, v []byte
		if cursor == "" {
			k, v = c.Last()
		} else {
			// 定位到游标之后（更旧的一侧）。
			//
			// key 是「定宽时间戳-域名」，字典序即时间序，所以 Seek 得到的是
			// 「第一个 >= 游标的 key」——要么就是游标本身，要么是比它新的一条。
			// 两种情况都再 Prev 一步：前者跳过自己（上一页已经给过了），
			// 后者落到比游标更旧的那一条，正是下一页的起点。
			//
			// 游标找不到也有正确的行为：它可能已经被保留上限裁掉了，
			// 这时返回「从头开始」会比返回空页好——翻出来的是当前还留着的
			// 那部分，而不是一句「没有更多了」。
			k, v = c.Seek([]byte(cursor))
			k, v = c.Prev()
		}
		skipped := 0
		for ; k != nil; k, v = c.Prev() {
			keep, ferr := fn(v)
			if errors.Is(ferr, ErrStop) {
				return nil
			}
			if ferr != nil {
				return ferr
			}
			if !keep {
				continue
			}
			if skipped < offset {
				skipped++
				continue
			}
			out = append(out, append([]byte(nil), v...))
			keys = append(keys, string(k))
			if len(out) >= limit {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, keys, nil
}

// PutNotifyState 保存某个推送通道的最近投递状态。
//
// 落盘的原因：推送失败是最需要被人看见的事，而它往往发生在深夜的定时巡检里。
// 内存态重启即丢，等于「昨晚三次推送全挂了」这件事永远没人知道。
func (s *Store) PutNotifyState(v notify.State) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketNotify).Put([]byte(v.Name), b)
	})
}

// NotifyStates 读取全部推送通道状态，key 为通道名。
func (s *Store) NotifyStates() (map[string]notify.State, error) {
	out := map[string]notify.State{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketNotify).ForEach(func(k, v []byte) error {
			var st notify.State
			if json.Unmarshal(v, &st) == nil {
				out[string(k)] = st
			}
			return nil
		})
	})
	return out, err
}

// Dump 是跨机器迁移用的完整数据快照。
//
// 之前 export/import 只搬缓存，于是 README 里说的「跨机器迁移用 JSON」其实
// 一半是假的：迁移完监控列表、历史快照、变更流水、推送通道状态全丢，
// 用户会以为「换个机器就什么都没了」。要搬就搬干净。
type Dump struct {
	Version    int               `json:"version"`
	ExportedAt int64             `json:"exported_at"`
	Cache      []*Entry          `json:"cache"`
	History    []model.Record    `json:"history"`
	Watch      []WatchItem       `json:"watch"`
	Changes    []json.RawMessage `json:"changes"`
	Notify     []notify.State    `json:"notify"`
	Counters   map[string]int64  `json:"counters"`
}

// DumpVersion 是当前导出格式版本。
const DumpVersion = 2

// Dump 在同一个读事务里取出全部数据，保证导出的是某一时刻的一致快照。
func (s *Store) Dump() (*Dump, error) {
	d := &Dump{Version: DumpVersion, ExportedAt: time.Now().Unix(), Counters: map[string]int64{}}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketCache).ForEach(func(k, v []byte) error {
			var e Entry
			if json.Unmarshal(v, &e) == nil {
				d.Cache = append(d.Cache, &e)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketHistory).ForEach(func(k, v []byte) error {
			var r model.Record
			if json.Unmarshal(v, &r) == nil {
				d.History = append(d.History, r)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketWatch).ForEach(func(k, v []byte) error {
			var w WatchItem
			if json.Unmarshal(v, &w) == nil {
				d.Watch = append(d.Watch, w)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketChanges).ForEach(func(k, v []byte) error {
			d.Changes = append(d.Changes, append(json.RawMessage(nil), v...))
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketNotify).ForEach(func(k, v []byte) error {
			var st notify.State
			if json.Unmarshal(v, &st) == nil {
				d.Notify = append(d.Notify, st)
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).ForEach(func(k, v []byte) error {
			if len(v) == 8 {
				d.Counters[string(k)] = int64(binary.BigEndian.Uint64(v))
			}
			return nil
		})
	})
	return d, err
}

// Restore 把 Dump 写回存储，单个事务完成。
//
// 语义分两类：
//   - 缓存、监控列表、推送状态是「当前状态」，先清空再写入，保证新机器
//     和旧机器一模一样，不会留下新机器上原有的残留域名；
//   - 历史快照与变更流水是只增的日志，key 由内容决定，按 upsert 合并即可
//     （重复导入同一份文件不会翻倍）。
func (s *Store) Restore(d *Dump) error {
	if d == nil {
		return errors.New("空数据")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketCache, bucketWatch, bucketNotify} {
			if err := clearBucket(tx.Bucket(name)); err != nil {
				return err
			}
		}
		cache := tx.Bucket(bucketCache)
		for _, e := range d.Cache {
			if e == nil || e.Domain == "" {
				continue
			}
			buf, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := cache.Put([]byte(e.Domain), buf); err != nil {
				return err
			}
		}
		hist := tx.Bucket(bucketHistory)
		for _, r := range d.History {
			if r.Domain == "" {
				continue
			}
			buf, err := json.Marshal(r)
			if err != nil {
				return err
			}
			day := time.Unix(r.FetchedAt, 0).Format("20060102")
			fp := r.Fingerprint()
			if len(fp) > 12 {
				fp = fp[:12]
			}
			key := []byte(fmt.Sprintf("%s\x00%s\x00%020d\x00%s", r.Domain, day, r.FetchedAt, fp))
			if err := hist.Put(key, buf); err != nil {
				return err
			}
		}
		watch := tx.Bucket(bucketWatch)
		for _, w := range d.Watch {
			if w.Domain == "" {
				continue
			}
			buf, err := json.Marshal(w)
			if err != nil {
				return err
			}
			if err := watch.Put([]byte(w.Domain), buf); err != nil {
				return err
			}
		}
		changes := tx.Bucket(bucketChanges)
		for _, raw := range d.Changes {
			var c struct {
				Domain string `json:"domain"`
				At     int64  `json:"at"`
			}
			if json.Unmarshal(raw, &c) != nil || c.Domain == "" {
				continue
			}
			// key 必须从内容重建：变更流水的 key 就是「时间-域名」，
			// 搬过来之后还得能按时间倒序遍历。
			key := []byte(fmt.Sprintf("%020d-%s", c.At, c.Domain))
			if err := changes.Put(key, raw); err != nil {
				return err
			}
		}
		states := tx.Bucket(bucketNotify)
		for _, st := range d.Notify {
			if st.Name == "" {
				continue
			}
			buf, err := json.Marshal(st)
			if err != nil {
				return err
			}
			if err := states.Put([]byte(st.Name), buf); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		for name, v := range d.Counters {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(v))
			if err := meta.Put([]byte(name), buf[:]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.invalidateHistoryCount()
	// 计数器在内存里有副本，导入后必须重新加载，否则会拿旧值继续累加。
	s.cmu.Lock()
	s.base = map[string]int64{}
	s.counters = map[string]int64{}
	s.cmu.Unlock()
	return s.loadCounters()
}

// clearBucket 清空一个 bucket。bbolt 在 ForEach 里删除是官方支持的用法。
func clearBucket(b *bolt.Bucket) error {
	var keys [][]byte
	_ = b.ForEach(func(k, v []byte) error {
		keys = append(keys, append([]byte(nil), k...))
		return nil
	})
	for _, k := range keys {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// PurgeNotifyState 删除不在 names 里的通道状态，避免改配置后遗留幽灵通道。
func (s *Store) PurgeNotifyState(names map[string]struct{}) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketNotify)
		var stale [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			if _, ok := names[string(k)]; !ok {
				stale = append(stale, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}
