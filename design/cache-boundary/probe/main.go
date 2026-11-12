// Package main 是缓存边界设计的一次性证据探针（不属于生产代码）。
//
// 它直接复用生产调用链 internal/render.Render 与 internal/audiocodec 的
// 导出变换，在不实现任何缓存的前提下，产出可重复的判据证据：
//   - 调用链确定性与探针基准正确性（experiment 0）
//   - 跨块接点（交叉淡化）使接点块必须随邻居失效（experiment 1）
//   - 远处更大峰值使全局归一化增益改变、远处最终 PCM 不能复用（experiment 2）
//   - 源字节损坏时完整性闸门先于任何缓存查询（experiment 3）
//   - 内容寻址 + 容量 LRU 的内存预算不随编辑次数增长（experiment 4）
//
// 运行方式（仓库根目录）：
//
//	go run ./design/cache-boundary/probe -exp all
//	go run ./design/cache-boundary/probe -exp exp1
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
	"voice-timeline-renderer/internal/storage"
)

const (
	rate       = 48000
	channels   = 1
	blockLen   = 2400 // 每块 50ms，全部实验共用同一固定网格；28800/2400=12 块
	blockCount = 12
	totalFrames = int64(28800)
	xfade      = int64(4800) // 一个接点恰好跨 2 个块，便于观察接点失效半径
	targetDB   = -1.0
)

type spanSpec struct {
	sourceID         string
	frames           int64
	gainDB           float64
	crossfadeFrames  int64
	startFrame       int64 // 输入源切片起点（本实验全部为 0，仅为显式）
}

type fixture struct {
	name   string
	plan   domain.Timeline
	inputs map[string]render.SourceInput
}

func main() {
	exp := flag.String("exp", "all", "选择实验: exp0|exp1|exp2|exp3|exp4|all")
	flag.Parse()

	ctx := context.Background()
	selected := map[string]bool{}
	if *exp == "all" {
		selected = map[string]bool{"exp0": true, "exp1": true, "exp2": true, "exp3": true, "exp4": true}
	} else {
		selected[*exp] = true
	}

	results := make([]string, 0)
	emit := func(line string) {
		fmt.Println(line)
		results = append(results, line)
	}

	type experiment struct {
		id  string
		run func(emit func(string)) error
	}
	experiments := []experiment{
		{"exp0", func(emit func(string)) error { return runExp0(ctx, emit) }},
		{"exp1", func(emit func(string)) error { return runExp1(ctx, emit) }},
		{"exp2", func(emit func(string)) error { return runExp2(ctx, emit) }},
		{"exp3", func(emit func(string)) error { return runExp3(ctx, emit) }},
		{"exp4", func(emit func(string)) error { return runExp4(emit) }},
	}

	failures := 0
	for _, e := range experiments {
		if !selected[e.id] {
			continue
		}
		emit("")
		emit("==== " + e.id + " ====")
		if err := e.run(emit); err != nil {
			failures++
			emit("FAIL  " + e.id + ": " + err.Error())
		} else {
			emit("PASS  " + e.id)
		}
	}

	if failures > 0 {
		os.Exit(1)
	}
}

// buildRender 走真实生产调用链：render.Render -> prepareSpans -> mixPrepared ->
// normalizePeak -> EncodePCM16 -> buildReceipt。
func buildRender(ctx context.Context, fx fixture) (render.Result, error) {
	return render.Render(ctx, "probe-render", fx.plan, fx.inputs, render.Limits{MaxOutputFrames: 48000 * 180}, time.Unix(1_700_000_000, 0).UTC())
}

func makeSource(id string, samples []float32, digestNote string) render.SourceInput {
	wav, err := audiocodec.EncodePCM16(rate, channels, samples)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(wav)
	digest := hex.EncodeToString(sum[:])
	clip, info, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		panic(err)
	}
	metrics := audiocodec.Measure(clip.Samples)
	_ = info
	_ = digestNote
	return render.SourceInput{
		Asset: domain.SourceAsset{
			ID:         id,
			Name:       id,
			Digest:     digest, // 真实摘要：实验 3 的完整性闸门依赖它
			FrameCount: clip.FrameCount(),
			SampleRate: rate,
			Channels:   channels,
			PeakDBFS:   metrics.PeakDBFS,
			RMSDBFS:    metrics.RMSDBFS,
		},
		WAV: wav,
	}
}

func makePlan(spanA, spanB, spanC spanSpec) domain.Timeline {
	return domain.Timeline{
		ID:             "probe-timeline",
		Name:           "probe timeline",
		SampleRate:     rate,
		OutputChannels: channels,
		TargetPeakDBFS: targetDB,
		Spans: []domain.TimelineSpan{
			{SourceID: spanA.sourceID, SourceStartFrame: spanA.startFrame, SourceEndFrame: spanA.startFrame + spanA.frames, GainDB: spanA.gainDB, CrossfadeFrames: spanA.crossfadeFrames},
			{SourceID: spanB.sourceID, SourceStartFrame: spanB.startFrame, SourceEndFrame: spanB.startFrame + spanB.frames, GainDB: spanB.gainDB, CrossfadeFrames: spanB.crossfadeFrames},
			{SourceID: spanC.sourceID, SourceStartFrame: spanC.startFrame, SourceEndFrame: spanC.startFrame + spanC.frames, GainDB: spanC.gainDB, CrossfadeFrames: spanC.crossfadeFrames},
		},
	}
}

func threeSpanSpecs() (spanSpec, spanSpec, spanSpec) {
	// 输出布局：A[0,9600)，B[4800,24000)（前 4800 与 A 淡化），
	// C[19200,28800)（前 4800 与 B 淡化），总长 28800 = 13*9600。
	a := spanSpec{sourceID: "src-a", frames: 9600, gainDB: 0, crossfadeFrames: 0}
	b := spanSpec{sourceID: "src-b", frames: 19200, gainDB: 0, crossfadeFrames: xfade}
	c := spanSpec{sourceID: "src-c", frames: 9600, gainDB: 0, crossfadeFrames: xfade}
	return a, b, c
}

func fixtureFromSamples(name string, aSamples, bSamples, cSamples []float32) fixture {
	a, b, c := threeSpanSpecs()
	return fixture{
		name: name,
		plan: makePlan(a, b, c),
		inputs: map[string]render.SourceInput{
			"src-a": makeSource("src-a", aSamples, "digest-a"),
			"src-b": makeSource("src-b", bSamples, "digest-b"),
			"src-c": makeSource("src-c", cSamples, "digest-c"),
		},
	}
}

// ---------- 信号构造 ----------

func sine(frames int64, freq float64, amp float32, phase float64) []float32 {
	out := make([]float32, frames)
	for i := int64(0); i < frames; i++ {
		out[i] = float32(math.Sin(2*math.Pi*freq*float64(i)/float64(rate)+phase)) * amp
	}
	return out
}

// withImpulse 在指定帧写入一个幅度为 amp 的单帧脉冲，其余保持原值。
func withImpulse(base []float32, at int64, amp float32) []float32 {
	out := append([]float32(nil), base...)
	out[at] = amp
	return out
}

// negate 返回逐样本取反的副本，峰值与 RMS 不变。
func negate(in []float32) []float32 {
	out := make([]float32, len(in))
	for i, v := range in {
		out[i] = -v
	}
	return out
}

// ---------- 探针基准：与生产配方逐项对齐的参考实现 ----------

type refPrepared struct {
	samples          []float32
	outputStartFrame int64
	frames           int64
	crossfadeFrames  int64
	gain             float32
}

// referencePrepareMix 严格复刻 prepareSpans 的导出变换序列与 mixPrepared 的
// 等功率交叉淡化公式，同时返回“归一化前”的 mix 缓冲。
func referencePrepareMix(fx fixture) ([]refPrepared, []float32) {
	ctx := context.Background()
	prepared := make([]refPrepared, 0, len(fx.plan.Spans))
	cursor := int64(0)
	for index, span := range fx.plan.Spans {
		in := fx.inputs[span.SourceID]
		clip, _, err := audiocodec.DecodeWAV(in.WAV)
		if err != nil {
			panic(err)
		}
		selected, err := audiocodec.SliceFrames(ctx, clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			panic(err)
		}
		resampled, err := audiocodec.Resample(ctx, selected, fx.plan.SampleRate)
		if err != nil {
			panic(err)
		}
		mapped, _, err := audiocodec.MapChannels(ctx, resampled, fx.plan.OutputChannels)
		if err != nil {
			panic(err)
		}
		frames := mapped.FrameCount()
		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		end := start + frames
		cursor = end
		prepared = append(prepared, refPrepared{
			samples:          mapped.Samples,
			outputStartFrame: start,
			frames:           frames,
			crossfadeFrames:  span.CrossfadeFrames,
			gain:             float32(audiocodec.DBToLinear(span.GainDB)),
		})
	}
	mix := make([]float32, cursor*int64(channels))
	for _, sp := range prepared {
		for frame := int64(0); frame < sp.frames; frame++ {
			incoming := float32(1)
			outgoing := float32(1)
			if sp.crossfadeFrames > 0 && frame < sp.crossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(sp.crossfadeFrames)
				angle := progress * math.Pi / 2
				incoming = float32(math.Sin(angle))
				outgoing = float32(math.Cos(angle))
			}
			outStart := int(sp.outputStartFrame+frame) * channels
			value := sp.samples[int(frame)*channels] * sp.gain * incoming
			if sp.crossfadeFrames > 0 && frame < sp.crossfadeFrames {
				mix[outStart] *= outgoing
			}
			mix[outStart] += value
		}
	}
	return prepared, mix
}

// referenceNormalize 复刻 normalizePeak 的增益选择与夹断逻辑。
func referenceNormalize(samples []float32, targetDBFS float64) ([]float32, float64) {
	target := audiocodec.DBToLinear(targetDBFS)
	peak := 0.0
	for _, s := range samples {
		if a := math.Abs(float64(s)); a > peak {
			peak = a
		}
	}
	gain := 1.0
	if peak > 0 {
		gain = target / peak
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	out := make([]float32, len(samples))
	for i, s := range samples {
		v := float64(s) * gain
		if v > 1 {
			v = 1
		}
		if v < -1 {
			v = -1
		}
		out[i] = float32(v)
	}
	return out, gain
}

func decodedSamples(wav []byte) []float32 {
	clip, _, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		panic(err)
	}
	return clip.Samples
}

// referencePCM16RoundTrip 复刻 EncodePCM16 的 16bit 量化再经 DecodeWAV
// 反量化的过程，使参考链路与生产链路在同一量化精度上可比。
func referencePCM16RoundTrip(samples []float32) []float32 {
	out := make([]float32, len(samples))
	for i, s := range samples {
		var quantized int32
		switch {
		case s <= -1:
			quantized = -32768
		case s >= 1:
			quantized = 32767
		case s < 0:
			quantized = int32(math.Round(float64(s) * 32768))
		default:
			quantized = int32(math.Round(float64(s) * 32767))
		}
		if quantized < -32768 {
			quantized = -32768
		}
		if quantized > 32767 {
			quantized = 32767
		}
		if quantized < 0 {
			out[i] = float32(quantized) / 32768
		} else {
			out[i] = float32(quantized) / 32767
		}
	}
	return out
}

// blockRange 返回固定网格第 block 索引对应的帧区间。
func blockRange(block int) (int64, int64) {
	start := int64(block) * blockLen
	end := start + blockLen
	if end > totalFrames {
		end = totalFrames
	}
	return start, end
}

func countDifferingBlocks(a, b []float32) (int, []int) {
	diffBlocks := make([]int, 0)
	for blk := 0; blk < blockCount; blk++ {
		s, e := blockRange(blk)
		diff := false
		for f := s; f < e; f++ {
			if a[f] != b[f] {
				diff = true
				break
			}
		}
		if diff {
			diffBlocks = append(diffBlocks, blk)
		}
	}
	return len(diffBlocks), diffBlocks
}

func countDifferingFrames(a, b []float32, start, end int64) int {
	n := 0
	for f := start; f < end; f++ {
		if a[f] != b[f] {
			n++
		}
	}
	return n
}

func peakOf(samples []float32, start, end int64) float64 {
	peak := 0.0
	for f := start; f < end; f++ {
		if a := math.Abs(float64(samples[f])); a > peak {
			peak = a
		}
	}
	return peak
}

// staleHybrid 模拟错误方案：只重算 changedBlocks 对应的块，其余块直接复用旧
// 最终 PCM（“按块缓存最终输出，改动块以外一律命中”）。
func staleHybrid(oldPCM, newPCM []float32, changedBlocks map[int]bool) []float32 {
	out := append([]float32(nil), oldPCM...)
	for blk := 0; blk < blockCount; blk++ {
		if changedBlocks[blk] {
			s, e := blockRange(blk)
			copy(out[s:e], newPCM[s:e])
		}
	}
	return out
}

func assert(cond bool, message string) error {
	if !cond {
		return errors.New(message)
	}
	return nil
}

// ensureRepoForCorruption 用真实 storage.Repository 落盘一个源，返回数据目录。
func ensureRepoForCorruption(sourceID string, wav []byte, asset domain.SourceAsset) (*storage.Repository, string, error) {
	dir, err := os.MkdirTemp("", "voice-cache-probe-*")
	if err != nil {
		return nil, "", err
	}
	repo, err := storage.NewRepository(dir)
	if err != nil {
		return nil, dir, err
	}
	if err := repo.SaveSource(asset, wav); err != nil {
		return nil, dir, err
	}
	return repo, dir, nil
}
