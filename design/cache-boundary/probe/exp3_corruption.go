package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// 实验 3：源字节损坏时，完整性闸门必须先于任何缓存查询。
//
// 生产现状（internal/storage/sources.go GetSource）：每次读取源都重新计算
// SHA-256 并与 metadata 中的 digest 比对，不一致返回 source_integrity_error。
// 本实验证明：
//  1. 损坏后真实存储层拒绝该源，渲染链路拿不到输入，无法交付旧成功。
//  2. 反证：如果系统先查内存缓存（键里只有“源 ID + 渲染参数”而没有已验证
//     摘要），损坏对缓存完全不可见，旧 PCM 会被继续交付 —— 这正是要禁止的顺序。
func runExp3(ctx context.Context, emit func(string)) error {
	a := sine(9600, 220, 0.10, 0)
	b := sine(19200, 330, 0.10, 0.4)
	c := sine(9600, 440, 0.10, 0.2)
	c[7200] = 0.9
	fx := fixtureFromSamples("exp3", a, b, c)

	// 用真实 Repository 落盘 src-b（模拟损坏发生在已成功渲染之后的磁盘位翻转）。
	srcB := fx.inputs["src-b"]
	repo, dir, err := ensureRepoForCorruption("src-b", srcB.WAV, srcB.Asset)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// 损坏前：完整性校验通过。
	if _, payload, err := repo.GetSource("src-b"); err != nil {
		return fmt.Errorf("损坏前读取意外失败: %w", err)
	} else {
		sum := sha256.Sum256(payload)
		emit(fmt.Sprintf("损坏前 GetSource 通过，payload 摘要=%s", hex.EncodeToString(sum[:8])))
	}

	// 直接翻转 data chunk 内一个数据字节（跳过 44 字节 WAV 头）。
	mediaPath := filepath.Join(dir, "sources", "src-b", "source.wav")
	raw, err := os.ReadFile(mediaPath)
	if err != nil {
		return err
	}
	if err := assert(len(raw) > 100, "fixture WAV 长度异常"); err != nil {
		return err
	}
	raw[60] ^= 0x01
	if err := os.WriteFile(mediaPath, raw, 0o640); err != nil {
		return err
	}

	// 损坏后：真实存储层必须拒绝。
	_, _, getErr := repo.GetSource("src-b")
	if getErr == nil {
		return fmt.Errorf("损坏后 GetSource 未报错，完整性闸门缺失")
	}
	appErr := domain.AsAppError(getErr)
	emit(fmt.Sprintf("损坏后 GetSource 拒绝: kind=%s code=%s", appErr.Kind, appErr.Detail.Code))
	if err := assert(appErr.Detail.Code == "source_integrity_error", "必须返回稳定错误码 source_integrity_error"); err != nil {
		return err
	}

	// 损坏后的 WAV 仍可能是结构合法、可解码的（仅一个样本位翻转），
	// 说明仅靠 DecodeWAV 成功不能作为继续交付的依据。
	if clip, _, decErr := audiocodec.DecodeWAV(raw); decErr == nil {
		emit(fmt.Sprintf("损坏 payload 仍可被 DecodeWAV 解码（%d 帧）：解码成功不等于字节可信", clip.FrameCount()))
	} else {
		emit("损坏 payload 解码失败（本 fixture 翻转的是数据字节，预期仍可解码）")
	}

	// 反证模拟：错误顺序“先查缓存”。键只用源 ID 与渲染参数，缓存保存着上一次
	// 成功渲染的输出。损坏发生后缓存仍然命中并直接返回旧 WAV。
	type unsafeKey struct {
		sourceID string
		params   string
	}
	unsafeCache := map[unsafeKey][]byte{}
	k := unsafeKey{sourceID: "src-b", params: "rate=48000/ch=1/gain=0"}
	r1, err := buildRender(ctx, fx)
	if err != nil {
		return err
	}
	unsafeCache[k] = r1.WAV // 上一次成功渲染入库

	if cached, hit := unsafeCache[k]; hit {
		sum := sha256.Sum256(cached)
		emit(fmt.Sprintf("错误顺序下缓存命中并交付旧输出 %s，完全绕过损坏事实", hex.EncodeToString(sum[:8])))
		if err := assert(hit, "反证场景必须复现错误命中"); err != nil {
			return err
		}
	}

	emit("结论: 缓存查询必须排在 GetSource 完整性校验之后；缓存键必须包含已验证的")
	emit("      SHA-256 摘要而非源 ID；任何引用源校验失败即整单渲染失败，禁止负缓存、")
	emit("      禁止回退到旧成功产物。")
	return nil
}
