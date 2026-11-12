package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// 实验 0：调用链确定性 + 探针基准正确性。
//
// 目标：
//  1. 同一输入两次走 render.Render，输出 WAV 字节级一致（旧工程 PCM 可复现的前提）。
//  2. 探针自带的参考实现（导出变换 + 与 mixPrepared/normalizePeak 同公式）
//     与真实调用链产出的解码样本逐样本一致，证明后续“归一化前”观测可信。
func runExp0(ctx context.Context, emit func(string)) error {
	a := sine(9600, 220, 0.10, 0)
	b := sine(19200, 330, 0.10, 0.4)
	c := sine(9600, 440, 0.10, 0.2)
	c[7200] = 0.9 // span C 内部的高幅度锚点，固定全局峰值
	fx := fixtureFromSamples("exp0", a, b, c)

	r1, err := buildRender(ctx, fx)
	if err != nil {
		return err
	}
	r2, err := buildRender(ctx, fx)
	if err != nil {
		return err
	}

	d1 := sha256.Sum256(r1.WAV)
	d2 := sha256.Sum256(r2.WAV)
	emit(fmt.Sprintf("两次渲染输出 SHA-256: %s", hex.EncodeToString(d1[:])))
	emit(fmt.Sprintf("两次渲染输出 SHA-256: %s", hex.EncodeToString(d2[:])))
	if err := assert(bytes.Equal(r1.WAV, r2.WAV), "同一输入两次渲染必须字节一致"); err != nil {
		return err
	}
	emit("确定性: 同一输入两次 render.Render 输出字节级一致")

	// 探针基准 vs 真实链路（同一 PCM16 量化精度上逐样本比较）。
	_, preMix := referencePrepareMix(fx)
	refNormalized, refGain := referenceNormalize(preMix, targetDB)
	refQuantized := referencePCM16RoundTrip(refNormalized)
	prodSamples := decodedSamples(r1.WAV)

	if err := assert(len(refQuantized) == len(prodSamples), "参考与生产输出长度不一致"); err != nil {
		return err
	}
	mismatch := 0
	for i := range refQuantized {
		if refQuantized[i] != prodSamples[i] {
			mismatch++
		}
	}
	emit(fmt.Sprintf("参考实现与生产链路逐样本比对(PCM16量化后): 总样本=%d 不一致=%d", len(prodSamples), mismatch))
	emit(fmt.Sprintf("参考增益(线性)=%.9f 回执 applied_gain_db=%.6f", refGain, r1.Receipt.Normalization.AppliedGainDB))
	if err := assert(mismatch == 0, "探针参考实现必须与生产链路逐样本一致"); err != nil {
		return err
	}
	emit(fmt.Sprintf("回执 before_peak_dbfs=%.6f applied_gain_db=%.6f output_peak_dbfs=%.6f",
		r1.Receipt.Normalization.BeforePeakDBFS,
		r1.Receipt.Normalization.AppliedGainDB,
		r1.Receipt.Normalization.OutputPeakDBFS))
	emit("基准成立: 探针参考实现与 render.Render 等价，后续 pre-norm 观测可作为缓存层证据")
	return nil
}

// 实验 1：跨块接点 —— 交叉淡化把邻居块与被编辑块耦合。
//
// 输出帧布局（48kHz 单声道，块长 9600/块）：
//   span A: [0,9600)，无淡化
//   span B: [4800,24000)，前 4800 帧与 A 等功率交叉淡化（接点 J1 [4800,9600)）
//   span C: [19200,28800)，前 4800 帧与 B 等功率交叉淡化（接点 J2 [19200,24000)）
//
// 编辑：B 源全部样本取反（峰值/RMS 完全不变），C 内部保留 0.9 锚点固定全局增益，
// 从而把任何差异精确归因于接点混合，而不是归一化增益变化。
// 错误方案 H1：“只让 B 自己覆盖的块失效，其余块沿用旧最终 PCM”。
func runExp1(ctx context.Context, emit func(string)) error {
	a := sine(9600, 220, 0.10, 0)
	b1 := sine(19200, 330, 0.10, 0.4)
	b2 := negate(b1)
	c := sine(9600, 440, 0.10, 0.2)
	c[7200] = 0.9 // 输出帧 26400，位于 span C 内部且远离 J2

	fx1 := fixtureFromSamples("exp1-r1", a, b1, c)
	fx2 := fixtureFromSamples("exp1-r2", a, b2, c)

	r1, err := buildRender(ctx, fx1)
	if err != nil {
		return err
	}
	r2, err := buildRender(ctx, fx2)
	if err != nil {
		return err
	}
	p1, p2 := decodedSamples(r1.WAV), decodedSamples(r2.WAV)

	emit(fmt.Sprintf("R1 before_peak=%.6f gain_db=%.6f | R2 before_peak=%.6f gain_db=%.6f",
		r1.Receipt.Normalization.BeforePeakDBFS, r1.Receipt.Normalization.AppliedGainDB,
		r2.Receipt.Normalization.BeforePeakDBFS, r2.Receipt.Normalization.AppliedGainDB))
	if err := assert(r1.Receipt.Normalization.AppliedGainDB == r2.Receipt.Normalization.AppliedGainDB,
		"本实验要求全局增益不变，以便把差异精确归因于接点混合而非归一化"); err != nil {
		return err
	}

	n, blocks := countDifferingBlocks(p1, p2)
	emit(fmt.Sprintf("真实重渲染差异块数=%d 块号=%v（50ms/块）", n, blocks))

	// 接点窗口（输出帧）。
	j1Start, j1End := int64(4800), int64(9600)
	j2Start, j2End := int64(19200), int64(24000)
	dJ1 := countDifferingFrames(p1, p2, j1Start, j1End)
	dJ2 := countDifferingFrames(p1, p2, j2Start, j2End)
	emit(fmt.Sprintf("接点 J1[4800,9600) 差异帧=%d/4800；接点 J2[19200,24000) 差异帧=%d/4800", dJ1, dJ2))

	// 远处内部区域必须逐样本一致。
	dAInterior := countDifferingFrames(p1, p2, 0, 4800)
	dCInterior := countDifferingFrames(p1, p2, 24000, totalFrames)
	emit(fmt.Sprintf("远处内部 A[0,4800) 差异帧=%d；C[24000,28800) 差异帧=%d", dAInterior, dCInterior))
	if err := assert(dAInterior == 0 && dCInterior == 0, "非接点、非编辑区域必须字节一致"); err != nil {
		return err
	}
	if err := assert(dJ1 > 0 && dJ2 > 0, "两个接点都必须检测到差异，证明交叉淡化的双向耦合"); err != nil {
		return err
	}

	// 归一化前层面的同样证据：接点 pre-norm 也不同。
	_, pre1 := referencePrepareMix(fx1)
	_, pre2 := referencePrepareMix(fx2)
	preJ1 := countDifferingFrames(pre1, pre2, j1Start, j1End)
	preJ2 := countDifferingFrames(pre1, pre2, j2Start, j2End)
	emit(fmt.Sprintf("pre-norm 层面 J1 差异帧=%d J2 差异帧=%d（归一化之前耦合已存在）", preJ1, preJ2))
	if err := assert(preJ1 > 0 && preJ2 > 0, "pre-norm 接点也必须不同"); err != nil {
		return err
	}

	// 错误方案 H1：按“span 自身的中间块”推断失效范围，只重算 B 中部不与任何
	// 邻居共享的块 4..7（[9600,19200)），承载接点的块 2/3（J1）与块 8/9（J2）
	// 以及 A/C 块全部沿用 R1 最终 PCM。真实差异块是 2..9，故 H1 必然在 4 个
	// 接点块上遗留陈旧淡化值。
	changedByH1 := map[int]bool{4: true, 5: true, 6: true, 7: true}
	hybrid := staleHybrid(p1, p2, changedByH1)
	hN, hBlocks := countDifferingBlocks(hybrid, p2)
	emit(fmt.Sprintf("错误方案 H1 拼装结果 vs 真实重渲染: 差异块=%d 块号=%v", hN, hBlocks))
	if err := assert(hN > 0, "H1 必须被证否：拼装结果与真实重渲染不一致"); err != nil {
		return err
	}
	staleJ1 := countDifferingFrames(hybrid, p2, j1Start, j1End)
	staleJ2 := countDifferingFrames(hybrid, p2, j2Start, j2End)
	emit(fmt.Sprintf("H1 遗留的接点错误帧: J1=%d J2=%d（接点块未失效，保留了旧淡化值）", staleJ1, staleJ2))
	if err := assert(staleJ1 > 0 && staleJ2 > 0, "H1 必须在两个接点处都留下可观测错误"); err != nil {
		return errors.New("H1 证否失败")
	}

	emit("结论: 缓存边界必须把交叉淡化窗口作为共享块；编辑 span 的失效半径包含两侧接点，")
	emit("      但 A 内部 [0,4800) 与 C 内部 [24000,28800) 仍可命中（本实验差异帧均为 0）")
	return nil
}
