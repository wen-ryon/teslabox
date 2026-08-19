// Package storagepolicy 集中定义外层数据盘为快照和初始化保留空间的规则。
package storagepolicy

import (
	"errors"
	"fmt"
	"math"
)

const (
	// DefaultMinFreeBytes 是当前快照运行期最低保留空间的保守默认值。
	DefaultMinFreeBytes uint64 = 8 * 1024 * 1024 * 1024
	// DefaultMinFreePercent 防止大容量盘只按固定字节数保留空间。
	DefaultMinFreePercent uint64 = 5
	// DefaultInitializationHeadroomBytes 避免新初始化设备紧贴运行期门槛。
	DefaultInitializationHeadroomBytes uint64 = 4 * 1024 * 1024 * 1024
)

// ReasonCode 是页面和诊断可稳定识别的空间策略结果。
type ReasonCode string

const (
	ReasonNone              ReasonCode = ""
	ReasonPolicyInvalid     ReasonCode = "policy_invalid"
	ReasonInsufficientSpace ReasonCode = "insufficient_space"
	ReasonPolicyUnreachable ReasonCode = "policy_unreachable"
)

// ErrPolicyInvalid 标识启动配置本身无效，与某一块设备容量不可达相区分。
var ErrPolicyInvalid = errors.New("快照空间策略配置无效")

// SnapshotReservePolicy 是进程生命周期内不可变的快照空间策略。
type SnapshotReservePolicy struct {
	minFreeBytes                uint64
	minFreePercent              uint64
	initializationHeadroomBytes uint64
}

// Evaluation 描述实际空间与策略要求之间的关系。
type Evaluation struct {
	TotalBytes    uint64
	FreeBytes     uint64
	RequiredBytes uint64
	DeficitBytes  uint64
	FreePercent   float64
	Ready         bool
	ReasonCode    ReasonCode
}

// NewSnapshotReservePolicy 校验一次启动配置，避免业务模块各自补默认值。
func NewSnapshotReservePolicy(minFreeBytes, minFreePercent, initializationHeadroomBytes uint64) (SnapshotReservePolicy, error) {
	if minFreeBytes == 0 {
		return SnapshotReservePolicy{}, fmt.Errorf("%w: 快照最低保留字节数必须大于零", ErrPolicyInvalid)
	}
	if minFreePercent == 0 || minFreePercent >= 100 {
		return SnapshotReservePolicy{}, fmt.Errorf("%w: 快照最低保留百分比必须在 1 到 99 之间", ErrPolicyInvalid)
	}
	if minFreeBytes > math.MaxUint64-initializationHeadroomBytes {
		return SnapshotReservePolicy{}, fmt.Errorf("%w: 快照最低保留空间与初始化余量之和溢出", ErrPolicyInvalid)
	}
	return SnapshotReservePolicy{
		minFreeBytes: minFreeBytes, minFreePercent: minFreePercent,
		initializationHeadroomBytes: initializationHeadroomBytes,
	}, nil
}

// DefaultSnapshotReservePolicy 返回产品尚未覆盖配置时使用的保守策略。
func DefaultSnapshotReservePolicy() SnapshotReservePolicy {
	policy, _ := NewSnapshotReservePolicy(DefaultMinFreeBytes, DefaultMinFreePercent, DefaultInitializationHeadroomBytes)
	return policy
}

// MinFreeBytes 返回运行期最低固定保留字节数。
func (p SnapshotReservePolicy) MinFreeBytes() uint64 { return p.minFreeBytes }

// MinFreePercent 返回运行期最低保留百分比。
func (p SnapshotReservePolicy) MinFreePercent() uint64 { return p.minFreePercent }

// InitializationHeadroomBytes 返回只用于新初始化验收的额外余量。
func (p SnapshotReservePolicy) InitializationHeadroomBytes() uint64 {
	return p.initializationHeadroomBytes
}

// RuntimeRequiredBytes 计算运行期最低保留空间。
func (p SnapshotReservePolicy) RuntimeRequiredBytes(totalBytes uint64) (uint64, error) {
	if err := p.validate(); err != nil {
		return 0, err
	}
	percentage := totalBytes / 100 * p.minFreePercent
	remainder := totalBytes % 100 * p.minFreePercent
	if remainder > 0 {
		percentage += (remainder + 99) / 100
	}
	return max(p.minFreeBytes, percentage), nil
}

// InitializationRequiredBytes 计算新初始化结束时必须保留的空间。
func (p SnapshotReservePolicy) InitializationRequiredBytes(totalBytes uint64) (uint64, error) {
	required, err := p.RuntimeRequiredBytes(totalBytes)
	if err != nil {
		return 0, err
	}
	if required > math.MaxUint64-p.initializationHeadroomBytes {
		return 0, errors.New("初始化保留空间计算溢出")
	}
	return required + p.initializationHeadroomBytes, nil
}

// EvaluateRuntime 只使用运行期门槛评价存量设备，不追溯要求初始化余量。
func (p SnapshotReservePolicy) EvaluateRuntime(totalBytes, freeBytes uint64) (Evaluation, error) {
	return p.evaluate(totalBytes, freeBytes, false)
}

// EvaluateInitialization 使用运行期门槛加初始化余量评价新初始化设备。
func (p SnapshotReservePolicy) EvaluateInitialization(totalBytes, freeBytes uint64) (Evaluation, error) {
	return p.evaluate(totalBytes, freeBytes, true)
}

func (p SnapshotReservePolicy) evaluate(totalBytes, freeBytes uint64, initialization bool) (Evaluation, error) {
	if totalBytes == 0 || freeBytes > totalBytes {
		return Evaluation{}, errors.New("文件系统空间统计无效")
	}
	required, err := p.RuntimeRequiredBytes(totalBytes)
	if initialization {
		required, err = p.InitializationRequiredBytes(totalBytes)
	}
	if err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{
		TotalBytes: totalBytes, FreeBytes: freeBytes, RequiredBytes: required,
		FreePercent: float64(freeBytes) * 100 / float64(totalBytes), Ready: freeBytes >= required,
	}
	if required >= totalBytes {
		result.Ready = false
		result.ReasonCode = ReasonPolicyUnreachable
	} else if !result.Ready {
		result.ReasonCode = ReasonInsufficientSpace
	}
	if freeBytes < required {
		result.DeficitBytes = required - freeBytes
	}
	return result, nil
}

func (p SnapshotReservePolicy) validate() error {
	if p.minFreeBytes == 0 || p.minFreePercent == 0 || p.minFreePercent >= 100 {
		return fmt.Errorf("快照空间策略未通过构造函数初始化")
	}
	return nil
}
