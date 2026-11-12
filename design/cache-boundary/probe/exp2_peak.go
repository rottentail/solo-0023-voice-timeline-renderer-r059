package main

import (
	"context"
	"fmt"
	"math"
)

// 实验 2：远处更大峰值 —— 全局峰值归一化让局部编辑改变所有远处样本。
//
// normalizePeak(mixer.go/normalize.go) 对整段 mix 求一个全局峰值并施加单一
// 增益 gain = target / globalPeak，因此任何块的最终 PCM 都是
// preNormBlock * globalGain（再夹断）。最终块 PCM 不能按块独立复用。
//
// 编辑：B 内部（远离两个接点）加入单帧 0.95 脉冲，使其 pre-norm 峰值超过
// 原本由 C 锚点保持的 0.9。接点与 A/C 的 pre-norm 样本完全不变，但全局增益改变。
// 错误方案 H2：“按块缓存最终 PCM，只重算编辑块，其余块命中旧缓存”。
func runExp2(ctx context.Context, emit func(string)) error {
	a := sine(9600, 220, 0.10, 0)
	b1 := sine(19200, 330, 0.10, 0.4)
	c := sine(9600, 440, 0.10, 0.2)
	c[7200] = 0.9 // R1 的全局峰值锚点（输出帧 26400，C 内部）

	// 编辑只发生在 B 的中部：输出帧 12000 对应 B 本地帧 12000-4800=7200，
	// 距 J1 末端(9600)与 J2 起点(19200)都很远，保证接点 pre-norm 不变。
	b2 := withImpulse(b1, 7200, 0.95)

	fx1 := fixtureFromSamples("exp2-r1", a, b1, c)
	fx2 := fixtureFromSamples("exp2-r2", a, b2, c)

	r1, err := buildRender(ctx, fx1)
	if err != nil {
		return err
	}
	r2, err := buildRender(ctx, fx2)
	if err != nil {
		return err
	}
	p1, p2 := decodedSamples(r1.WAV), decodedSamples(r2.WAV)

	g1 := r1.Receipt.Normalization.AppliedGainDB
	g2 := r2.Receipt.Normalization.AppliedGainDB
	emit(fmt.Sprintf("R1 before_peak_dbfs=%.6f applied_gain_db=%.6f", r1.Receipt.Normalization.BeforePeakDBFS, g1))
	emit(fmt.Sprintf("R2 before_peak_dbfs=%.6f applied_gain_db=%.6f", r2.Receipt.Normalization.BeforePeakDBFS, g2))
	if err := assert(g1 != g2, "远处出现更大峰值后，全局归一化增益必须改变"); err != nil {
		return err
	}

	// pre-norm 层面：A、C 与两个接点必须逐样本不变；只有 B 内部变化。
	_, pre1 := referencePrepareMix(fx1)
	_, pre2 := referencePrepareMix(fx2)
	dPreA := countDifferingFrames(pre1, pre2, 0, 4800)
	dPreJ1 := countDifferingFrames(pre1, pre2, 4800, 9600)
	dPreC := countDifferingFrames(pre1, pre2, 24000, totalFrames)
	dPreJ2 := countDifferingFrames(pre1, pre2, 19200, 24000)
	emit(fmt.Sprintf("pre-norm 差异帧: A内部=%d J1=%d J2=%d C内部=%d（预期全 0）", dPreA, dPreJ1, dPreJ2, dPreC))
	if err := assert(dPreA == 0 && dPreJ1 == 0 && dPreC == 0 && dPreJ2 == 0,
		"pre-norm 层远处与接点必须完全不变，证明变化只来自全局增益"); err != nil {
		return err
	}

	// pre-norm 块峰值汇总（两阶段缓存的依据）：除 B 中部块外峰值全部可复用，
	// 但汇总出的全局峰值改变。
	emit("pre-norm 各块峰值（块: R1 -> R2）:")
	globalPeak1, globalPeak2 := 0.0, 0.0
	for blk := 0; blk < blockCount; blk++ {
		s, e := blockRange(blk)
		pk1, pk2 := peakOf(pre1, s, e), peakOf(pre2, s, e)
		globalPeak1 = math.Max(globalPeak1, pk1)
		globalPeak2 = math.Max(globalPeak2, pk2)
		marker := ""
		if pk1 != pk2 {
			marker = "  <== 仅该块峰值变化"
		}
		emit(fmt.Sprintf("  block %02d [%5d,%5d): %.6f -> %.6f%s", blk, s, e, pk1, pk2, marker))
	}
	emit(fmt.Sprintf("块峰值汇总全局峰值: %.6f -> %.6f（局部块变化即改变全局增益）", globalPeak1, globalPeak2))

	// 最终 PCM 层面：远处 A/C 内部几乎所有非零样本都因增益变化而不同
	// （正弦过零点的量化零值天然相等，故按比例断言而非逐帧全等）。
	dA := countDifferingFrames(p1, p2, 0, 4800)
	dC := countDifferingFrames(p1, p2, 24000, totalFrames)
	emit(fmt.Sprintf("最终 PCM 远处差异帧: A内部=%d/4800 C内部=%d/4800（过零零值除外应全部不同）", dA, dC))
	if err := assert(float64(dA) > 4800*0.95 && float64(dC) > 4800*0.95, "最终 PCM 层面远处必须近乎全部失效"); err != nil {
		return err
	}

	// 对照：在归一化后的浮点样本（量化前）上取远处非零帧，新旧比值应恰好等于
// 两个线性增益之比，说明差异 100% 可归因于全局增益，而非混合内容。
	lin1 := math.Pow(10, g1/20)
	lin2 := math.Pow(10, g2/20)
	norm1, _ := referenceNormalize(pre1, targetDB)
	norm2, _ := referenceNormalize(pre2, targetDB)
	probeFrame := int64(100) // A 内部远处帧
	ratio := float64(norm2[probeFrame]) / float64(norm1[probeFrame])
	emit(fmt.Sprintf("远处帧 %d 浮点样本比值 n2/n1=%.9f，增益比 lin2/lin1=%.9f", probeFrame, ratio, lin2/lin1))
	if err := assert(math.Abs(ratio-(lin2/lin1)) < 1e-6, "浮点样本比值必须精确等于增益比"); err != nil {
		return err
	}
	// 量化域同一结论：大样本帧（C 的 0.9 锚点，输出帧 26400）的相对误差应在
	// 16bit 量化量级内。
	qRatio := float64(p2[26400]) / float64(p1[26400])
	emit(fmt.Sprintf("量化域锚点帧 26400 比值=%.9f（与增益比偏差 %.2e，处于 16bit 量化量级）", qRatio, math.Abs(qRatio-lin2/lin1)))
	if err := assert(math.Abs(qRatio-(lin2/lin1)) < 2e-4, "量化域比值也应逼近增益比"); err != nil {
		return err
	}

	// 错误方案 H2：只重算脉冲所在块（输出帧 12000 -> 块 5）。
	impulseOutputFrame := int64(12000)
	changedBlock := int(impulseOutputFrame / blockLen)
	hybrid := staleHybrid(p1, p2, map[int]bool{changedBlock: true})
	hN, hBlocks := countDifferingBlocks(hybrid, p2)
	emit(fmt.Sprintf("错误方案 H2（仅失效块 %d）vs 真实重渲染: 差异块=%d 块号=%v", changedBlock, hN, hBlocks))
	if err := assert(hN == blockCount-1, "H2 必须在其余全部远处块上遗留错误"); err != nil {
		return err
	}

	// 关键证否：H2 拼装输出的峰值与真实重渲染完全相同（旧块用旧增益、新块含
	// 新峰值，新峰值经旧增益缩放后仍恰好落在目标上），仅靠峰值/RMS 级指标的
	// 冒烟校验无法发现它是错的，必须逐样本比对。
	hybridPeak := peakOf(hybrid, 0, totalFrames)
	truePeak := peakOf(p2, 0, totalFrames)
	emit(fmt.Sprintf("H2 拼装输出峰值=%.6f 真实重渲染峰值=%.6f 目标=%.6f（三者重合，指标级校验漏检）",
		hybridPeak, truePeak, math.Pow(10, targetDB/20)))
	if err := assert(math.Abs(hybridPeak-truePeak) < 1e-6, "峰值指标无法识别 H2 的陈旧块（证否其充分性）"); err != nil {
		return err
	}
	if err := assert(countDifferingFrames(hybrid, p2, 0, 4800) > 4800*0.95, "H2 远处块必须与真实输出近乎逐样本不同"); err != nil {
		return err
	}

	// 正确方案对照：两阶段（pre-norm 块 + 块峰值汇总）。R2 的 pre-norm 远处块
	// 直接命中 R1，只重算块 5，再用新全局增益做一次 O(N) 的廉价缩放，量化后
	// 结果必须与真实 R2 逐样本一致。
	rebuilt := make([]float32, len(pre2))
	newGain := lin2
	for i := range rebuilt {
		v := float64(pre2[i]) * newGain
		if v > 1 {
			v = 1
		}
		if v < -1 {
			v = -1
		}
		rebuilt[i] = float32(v)
	}
	rebuiltQ := referencePCM16RoundTrip(rebuilt)
	mismatch := 0
	for i := range rebuiltQ {
		if rebuiltQ[i] != p2[i] {
			mismatch++
		}
	}
	emit(fmt.Sprintf("两阶段方案重建 R2: 逐样本不一致=%d（pre-norm 远处全命中，仅缩放遍历来一次）", mismatch))
	if err := assert(mismatch == 0, "两阶段方案必须与真实重渲染逐样本一致"); err != nil {
		return err
	}

	emit("结论: 最终 PCM 块不能作为缓存复用单位；可复用的是 pre-norm 块与块峰值，")
	emit("      全局增益变化时远处 pre-norm 缓存仍有效，只需重放统一缩放（廉价 O(N)）")
	return nil
}
