package main

import (
	"container/list"
	"fmt"
	"strings"
)

// 实验 4：内存预算不得随历史编辑次数无限增长。
//
// 模拟试听预览缓存的内容寻址 L1：条目是“已验证源摘要 + 变换参数”键控的
// prepared clip（对应 prepareSpans 中 Decode/Slice/Resample/MapChannels 的产物）。
// 同一份源内容无论被多少个工程版本、多少次编辑引用，都只占一份内存；
// 超过字节预算时按 LRU 淘汰。对照朴素策略：每次编辑生成一份新条目且永不淘汰。
type clipEntry struct {
	key  string
	size int // 近似字节数 = float32 样本数 * 4
}

type boundedCache struct {
	budgetBytes int
	usedBytes   int
	items       map[string]*list.Element
	order       *list.List
	evictions   int
}

func newBoundedCache(budget int) *boundedCache {
	return &boundedCache{budgetBytes: budget, items: map[string]*list.Element{}, order: list.New()}
}

func (c *boundedCache) put(e clipEntry) (hit bool) {
	if el, ok := c.items[e.key]; ok {
		c.order.MoveToFront(el)
		return true
	}
	for c.usedBytes+e.size > c.budgetBytes && c.order.Len() > 0 {
		oldest := c.order.Back()
		old := oldest.Value.(clipEntry)
		c.order.Remove(oldest)
		delete(c.items, old.key)
		c.usedBytes -= old.size
		c.evictions++
	}
	el := c.order.PushFront(e)
	c.items[e.key] = el
	c.usedBytes += e.size
	return false
}

// 朴素缓存：键含编辑序号，条目只增不减，内存随编辑历史线性增长。
type naiveCache struct {
	usedBytes int
	entries   int
}

func (n *naiveCache) put(e clipEntry) {
	n.usedBytes += e.size
	n.entries++
}

func runExp4(emit func(string)) error {
	const edits = 200
	// 每次编辑产生 3 个 span 的 prepared clip；其中 src-a/src-c 内容不变，
	// src-b 每次编辑内容变化（模拟反复试听）。单 clip 约 19200*4=76800 字节。
	clipSize := func(frames int64) int { return int(frames) * channels * 4 }
	sizeA, sizeB, sizeC := clipSize(9600), clipSize(19200), clipSize(9600)

	// 预算约容纳 16 个 clip：(38400+76800+38400)*16/3 量级，取 1.2MiB。
	budget := 1200 * 1024
	bounded := newBoundedCache(budget)
	naive := &naiveCache{}

	contentHits := 0
	for edit := 0; edit < edits; edit++ {
		keys := []clipEntry{
			{key: fmt.Sprintf("sha256:A|%d|%d|%d", rate, channels, 0), size: sizeA},
			{key: fmt.Sprintf("sha256:B-v%d|%d|%d|%d", edit, rate, channels, 0), size: sizeB},
			{key: fmt.Sprintf("sha256:C|%d|%d|%d", rate, channels, 0), size: sizeC},
		}
		for _, e := range keys {
			if bounded.put(e) {
				contentHits++
			}
			naive.put(e)
		}
	}

	liveEntries := bounded.order.Len()
	emit(fmt.Sprintf("编辑轮次=%d，每轮 3 个 span 请求；预算=%d 字节", edits, budget))
	emit(fmt.Sprintf("朴素只增缓存: 条目=%d 占用=%d 字节（随编辑次数线性无界增长）", naive.entries, naive.usedBytes))
	emit(fmt.Sprintf("内容寻址+LRU: 存活条目=%d 占用=%d 字节 淘汰次数=%d 内容命中=%d",
		liveEntries, bounded.usedBytes, bounded.evictions, contentHits))

	if err := assert(bounded.usedBytes <= budget, "LRU 占用必须始终不超过预算"); err != nil {
		return err
	}
	if err := assert(liveEntries < edits*3, "存活条目不得随编辑次数增长"); err != nil {
		return err
	}
	// A、C 内容键稳定，第一轮之后每轮都应命中（共 (edits-1)*2 次）。
	wantHits := (edits - 1) * 2
	emit(fmt.Sprintf("未改动源 A/C 的内容命中预期=%d 实际=%d", wantHits, contentHits))
	if err := assert(contentHits == wantHits, "未改动内容必须稳定命中，证明内存与编辑历史解耦"); err != nil {
		return err
	}
	if err := assert(naive.usedBytes > budget*20, "对照项必须显著超预算，凸显无界增长问题"); err != nil {
		return err
	}

	// 增长曲线采样：证明有界缓存对编辑次数趋于常数。
	var curve []string
	c2 := newBoundedCache(budget)
	for edit := 0; edit < edits; edit++ {
		c2.put(clipEntry{key: fmt.Sprintf("sha256:B-v%d", edit), size: sizeB})
		if edit == 0 || edit == 9 || edit == 49 || edit == 99 || edit == 199 {
			curve = append(curve, fmt.Sprintf("edit=%d 存活条目=%d 占用=%d", edit+1, c2.order.Len(), c2.usedBytes))
		}
	}
	emit("有界缓存增长曲线: " + strings.Join(curve, " | "))
	emit("结论: 键用内容摘要而非编辑序号；容量按字节预算 LRU 淘汰；内存上界是预算常数，")
	emit("      与历史编辑次数无关。失败渲染不写缓存（无负缓存），淘汰仅丢可重建的派生物。")
	return nil
}
