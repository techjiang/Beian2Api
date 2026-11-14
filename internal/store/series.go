package store

import (
	"encoding/json"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

// 这个文件是「按小时聚合的时间序列」。
//
// 为什么要有：后台概览页原来只有「此刻」——命中率是一个总数、缓存是一行
// 数字、变更是一条流水。全都是瞬时值。于是「命中率是 24% 还是 90%」看得见，
// 但「它是这两天才掉下来的，还是一直这样」看不见；缓存涨到 3000 条是正常
// 增长还是有人在刷也看不出。这些答案不在别处，就在库里，只是从来没被按时间
// 切过。
//
// 为什么不用 Prometheus / 时序库：这套系统的承诺是单二进制、一个数据文件、
// 备份等于复制。为了几张图引入一个时序库，等于把「小白能装」这件事卖掉。
// 每小时一行、只留 7 天，总量上限就是 168 个键——这个量级塞进 bbolt
// 顺手就存了，连索引都不需要。

// bucketSeries = []byte("series") // 小时时间戳(定宽字符串) -> SeriesPoint
var bucketSeries = []byte("series")

// SeriesRetainHours 是时间序列的保留窗口：7 天。
//
// 168 个键 × 每键 ~120 字节 ≈ 20KB。相比之下「不裁剪」的代价是无限增长，
// 而超过一周的分钟级趋势本来也没人对着看——图上一条线拉过 30 天，
// 分辨率还不如直接看这一周的。
const SeriesRetainHours = 168

// SeriesPoint 是一个小时的聚合结果。
//
// Cache 是这个小时的**最后观测值**（gauge），其余是这一小时内的累加（counter）。
// 两者混在一行里，是因为它们本来就是同一张图上的两条线：查询量回答「忙不忙」，
// 缓存量回答「攒了多少」。分两个 bucket 存反而要两次读、两次对齐时间轴。
type SeriesPoint struct {
	Hour    int64 `json:"hour"` // Unix 秒，整点
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	Stale   int64 `json:"stale"`
	Changes int64 `json:"changes"`
	Cache   int   `json:"cache"`
}

// Total 返回这一小时的总请求数（命中 + 未命中）。
func (p SeriesPoint) Total() int64 { return p.Hits + p.Misses }

// HitRate 返回这一小时的缓存命中率（百分比，0~100）。没有请求时返回 -1，
// 由调用方决定「无数据」怎么画——画成 0% 会让人以为命中率掉到零。
func (p SeriesPoint) HitRate() float64 {
	if t := p.Total(); t > 0 {
		return float64(p.Hits) * 100 / float64(t)
	}
	return -1
}

// seriesFields 把计数器名映射到序列字段。
//
// 只有这几个计数器进时间序列：命中/回退/变更。其余（比如未来的某个
// 自定义计数）留在 meta 里就够了——时间序列的价值在于「几条关键曲线」，
// 把每个计数器都摊成一行只是把 meta bucket 复制一遍。
var seriesFields = map[string]func(*SeriesPoint, int64){
	"hits":    func(p *SeriesPoint, d int64) { p.Hits += d },
	"misses":  func(p *SeriesPoint, d int64) { p.Misses += d },
	"stale":   func(p *SeriesPoint, d int64) { p.Stale += d },
	"changes": func(p *SeriesPoint, d int64) { p.Changes += d },
}

// hourKey 把 Unix 秒归到整点，并转成定宽 key（字典序即时间序）。
func hourKey(ts int64) int64 { return ts - ts%3600 }

func seriesKey(hour int64) []byte {
	return []byte(strconv.FormatInt(hour, 10))
}

// bumpSeries 在内存里累加当前小时的序列增量。
//
// 和 Incr 同一个理由：写一次 bbolt 要 fsync（实测 ~2.5ms），而缓存命中
// 本身是微秒级的事。序列跟着 15 秒的 Flush 一起落盘就够了——丢 15 秒的
// 曲线数据，没人看得出来。
func (s *Store) bumpSeries(name string, delta int64) {
	fn, ok := seriesFields[name]
	if !ok || delta == 0 {
		return
	}
	h := hourKey(nowUnix())
	s.seriesMu.Lock()
	p := s.seriesPending[h]
	if p == nil {
		p = &SeriesPoint{Hour: h}
		s.seriesPending[h] = p
	}
	fn(p, delta)
	s.seriesMu.Unlock()
}

// flushSeries 把内存里的序列增量写进 series bucket，并裁掉超出窗口的小时。
//
// 和计数器的 Flush 共用一次写事务：一次 fsync 写完两样东西，
// 而不是让维护循环为「曲线」多付一次磁盘同步。
func (s *Store) flushSeries(tx *bolt.Tx) error {
	s.seriesMu.Lock()
	pending := s.seriesPending
	s.seriesPending = map[int64]*SeriesPoint{}
	s.seriesMu.Unlock()

	if len(pending) == 0 {
		return nil
	}
	b := tx.Bucket(bucketSeries)
	for hour, delta := range pending {
		var cur SeriesPoint
		if v := b.Get(seriesKey(hour)); v != nil {
			_ = json.Unmarshal(v, &cur)
		}
		cur.Hour = hour
		cur.Hits += delta.Hits
		cur.Misses += delta.Misses
		cur.Stale += delta.Stale
		cur.Changes += delta.Changes
		v, err := json.Marshal(cur)
		if err != nil {
			continue
		}
		if err := b.Put(seriesKey(hour), v); err != nil {
			// 写失败要把增量放回去，否则这 15 秒的曲线就永久丢了。
			s.seriesMu.Lock()
			back := s.seriesPending[hour]
			if back == nil {
				back = &SeriesPoint{Hour: hour}
				s.seriesPending[hour] = back
			}
			back.Hits += delta.Hits
			back.Misses += delta.Misses
			back.Stale += delta.Stale
			back.Changes += delta.Changes
			s.seriesMu.Unlock()
		}
	}
	return s.trimSeries(b, hourKey(nowUnix()))
}

// trimSeries 删掉保留窗口之外的小时。
//
// key 是定宽十进制时间戳，游标 First() 就是最旧的一条，所以从头上删到
// 窗口内即停——不扫全表，最多扫到第一条有效记录。
func (s *Store) trimSeries(b *bolt.Bucket, cur int64) error {
	cutoff := cur - SeriesRetainHours*3600
	c := b.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.First() {
		hour, err := strconv.ParseInt(string(k), 10, 64)
		if err != nil {
			// 解析不了的键不动它：删数据的前提是能确定它是什么。
			break
		}
		if hour >= cutoff {
			break
		}
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// ObserveCacheSize 记录当前缓存条目数到本小时的 gauge。
//
// 单独一个方法而不是在每次 PutCache 里顺手记：PutCache 是热路径，
// 而「这一小时的缓存量」只需要一个观测值——每隔一分钟取一次和每秒取一次，
// 画出来的折线一模一样。
func (s *Store) ObserveCacheSize(n int) {
	h := hourKey(nowUnix())
	s.seriesMu.Lock()
	p := s.seriesPending[h]
	if p == nil {
		p = &SeriesPoint{Hour: h}
		s.seriesPending[h] = p
	}
	p.Cache = n
	s.seriesMu.Unlock()
}

// Series 返回最近 hours 个小时的序列，按时间升序，缺失的小时补零。
//
// 补零是必须的：前端画图要一条完整的时间轴。只返回有数据的小时，
// 图上的间距就不再代表时间——「两小时前」和「三天前」会挨在一起，
// 一条上升曲线可能是假的。
func (s *Store) Series(hours int) ([]SeriesPoint, error) {
	if hours <= 0 {
		hours = 24
	}
	if hours > SeriesRetainHours {
		hours = SeriesRetainHours
	}
	cur := hourKey(nowUnix())
	from := cur - int64(hours-1)*3600

	byHour := make(map[int64]SeriesPoint, hours)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSeries)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(seriesKey(from)); k != nil; k, v = c.Next() {
			hour, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil || hour > cur {
				continue
			}
			var p SeriesPoint
			if json.Unmarshal(v, &p) == nil {
				p.Hour = hour
				byHour[hour] = p
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 把还没落盘的那部分叠上去。否则图上的「最近一小时」永远是空的——
	// 而人看趋势图，第一眼就是看最近这一段。
	s.seriesMu.Lock()
	for hour, d := range s.seriesPending {
		p := byHour[hour]
		p.Hour = hour
		p.Hits += d.Hits
		p.Misses += d.Misses
		p.Stale += d.Stale
		p.Changes += d.Changes
		if d.Cache > 0 {
			p.Cache = d.Cache
		}
		byHour[hour] = p
	}
	s.seriesMu.Unlock()

	out := make([]SeriesPoint, 0, hours)
	for i := 0; i < hours; i++ {
		h := from + int64(i)*3600
		p, ok := byHour[h]
		if !ok {
			p = SeriesPoint{Hour: h}
		}
		out = append(out, p)
	}
	return out, nil
}

// nowUnix 是序列自己的时间来源。不共用 model.Now：
// 那个是给业务逻辑（抓取时间、事件时间）用的，测试里会被替换成固定值；
// 时间序列要的是「真实的这一小时」，跟着业务时间冻结会画出平坦的假曲线。
func nowUnix() int64 { return time.Now().Unix() }
