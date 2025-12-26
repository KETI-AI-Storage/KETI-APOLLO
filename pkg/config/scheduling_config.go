// ============================================
// APOLLO Scheduling Configuration
// 스케줄링 관련 설정값 (환경변수로 오버라이드 가능)
// ============================================

package config

import (
	"os"
	"strconv"
)

// SchedulingConfig holds all configurable scheduling parameters
type SchedulingConfig struct {
	// 기본 점수 설정
	DefaultPriority int // 기본 우선순위 (default: 50)
	BaseNodeScore   int // 노드 기본 점수 (default: 50)

	// 점수 임계값
	PreferThreshold int // "Prefer" 결정을 위한 점수 임계값 (default: 80)

	// 보너스 점수
	GPUBonusScore         int // GPU 가용시 보너스 (default: 30)
	CSDBonusScore         int // CSD 가용시 보너스 (default: 20)
	FastStorageBonusScore int // 빠른 스토리지 보너스 (default: 10)
	CPUBonusScore         int // CPU 여유시 보너스 (default: 5)
	MemoryBonusScore      int // 메모리 여유시 보너스 (default: 5)

	// 리소스 임계값
	CPUAvailableThreshold    float64 // CPU 보너스 기준 (cores, default: 2)
	MemoryAvailableThreshold int64   // 메모리 보너스 기준 (bytes, default: 4GB)
	CSDUtilizationThreshold  float64 // CSD 사용률 임계값 (%, default: 80)
	StorageUtilizationThreshold float64 // 스토리지 사용률 임계값 (%, default: 70)

	// I/O 패턴별 IOPS 요구사항
	ReadHeavyIOPS   int64 // 읽기 중심 IOPS (default: 10000)
	WriteHeavyIOPS  int64 // 쓰기 중심 IOPS (default: 5000)
	BurstyIOPS      int64 // 버스트 IOPS (default: 3000)
	DefaultIOPS     int64 // 기본 IOPS (default: 1000)

	// I/O 패턴별 처리량 요구사항 (MB/s)
	ReadHeavyThroughput   int64 // 읽기 중심 처리량 (default: 500)
	WriteHeavyThroughput  int64 // 쓰기 중심 처리량 (default: 300)
	BurstyThroughput      int64 // 버스트 처리량 (default: 200)
	DefaultThroughput     int64 // 기본 처리량 (default: 100)

	// 캐시 설정
	PolicyCacheTTLMinutes int // 정책 캐시 TTL (분, default: 5)
}

// DefaultSchedulingConfig returns default configuration
func DefaultSchedulingConfig() *SchedulingConfig {
	return &SchedulingConfig{
		// 기본 점수
		DefaultPriority: 50,
		BaseNodeScore:   50,

		// 임계값
		PreferThreshold: 80,

		// 보너스 점수
		GPUBonusScore:         30,
		CSDBonusScore:         20,
		FastStorageBonusScore: 10,
		CPUBonusScore:         5,
		MemoryBonusScore:      5,

		// 리소스 임계값
		CPUAvailableThreshold:      2,
		MemoryAvailableThreshold:   4 * 1024 * 1024 * 1024, // 4GB
		CSDUtilizationThreshold:    80,
		StorageUtilizationThreshold: 70,

		// IOPS
		ReadHeavyIOPS:  10000,
		WriteHeavyIOPS: 5000,
		BurstyIOPS:     3000,
		DefaultIOPS:    1000,

		// 처리량
		ReadHeavyThroughput:  500,
		WriteHeavyThroughput: 300,
		BurstyThroughput:     200,
		DefaultThroughput:    100,

		// 캐시
		PolicyCacheTTLMinutes: 5,
	}
}

// LoadSchedulingConfigFromEnv loads configuration from environment variables
func LoadSchedulingConfigFromEnv() *SchedulingConfig {
	cfg := DefaultSchedulingConfig()

	// 기본 점수
	if v := os.Getenv("SCHED_DEFAULT_PRIORITY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.DefaultPriority = i
		}
	}
	if v := os.Getenv("SCHED_BASE_NODE_SCORE"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.BaseNodeScore = i
		}
	}

	// 임계값
	if v := os.Getenv("SCHED_PREFER_THRESHOLD"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.PreferThreshold = i
		}
	}

	// 보너스 점수
	if v := os.Getenv("SCHED_GPU_BONUS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.GPUBonusScore = i
		}
	}
	if v := os.Getenv("SCHED_CSD_BONUS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.CSDBonusScore = i
		}
	}
	if v := os.Getenv("SCHED_STORAGE_BONUS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.FastStorageBonusScore = i
		}
	}
	if v := os.Getenv("SCHED_CPU_BONUS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.CPUBonusScore = i
		}
	}
	if v := os.Getenv("SCHED_MEMORY_BONUS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MemoryBonusScore = i
		}
	}

	// 리소스 임계값
	if v := os.Getenv("SCHED_CPU_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.CPUAvailableThreshold = f
		}
	}
	if v := os.Getenv("SCHED_MEMORY_THRESHOLD_GB"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MemoryAvailableThreshold = int64(i) * 1024 * 1024 * 1024
		}
	}
	if v := os.Getenv("SCHED_CSD_UTIL_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.CSDUtilizationThreshold = f
		}
	}
	if v := os.Getenv("SCHED_STORAGE_UTIL_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.StorageUtilizationThreshold = f
		}
	}

	// IOPS
	if v := os.Getenv("SCHED_READ_HEAVY_IOPS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ReadHeavyIOPS = i
		}
	}
	if v := os.Getenv("SCHED_WRITE_HEAVY_IOPS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.WriteHeavyIOPS = i
		}
	}
	if v := os.Getenv("SCHED_BURSTY_IOPS"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.BurstyIOPS = i
		}
	}

	// 처리량
	if v := os.Getenv("SCHED_READ_HEAVY_THROUGHPUT"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.ReadHeavyThroughput = i
		}
	}
	if v := os.Getenv("SCHED_WRITE_HEAVY_THROUGHPUT"); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.WriteHeavyThroughput = i
		}
	}

	// 캐시
	if v := os.Getenv("SCHED_POLICY_CACHE_TTL"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.PolicyCacheTTLMinutes = i
		}
	}

	return cfg
}
