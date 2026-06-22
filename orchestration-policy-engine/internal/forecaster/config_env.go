/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package forecaster

import (
	"os"
	"strconv"
)

// parseEnvFloat는 env 문자열을 float로 파싱하고, 비어 있거나 실패 시 defaultValue를 반환한다.
func parseEnvFloat(key string, defaultValue float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return defaultValue
	}
	return f
}

// LoadThresholdConfigFromEnv는 DefaultThresholdConfig를 시작점으로 하여
// FORECASTER_* 환경변수로 개별 임계치를 덮어쓴다.
// ConfigMap을 envFrom으로 주입하면 운영에서 튜닝 가능하다.
//
// Parameters:
// - 없음 (환경변수만 읽음)
//
// Returns:
// - ThresholdConfig: 병합된 설정
func LoadThresholdConfigFromEnv() ThresholdConfig {
	t := DefaultThresholdConfig()
	t.CPUWarning = parseEnvFloat("FORECASTER_CPU_WARNING", t.CPUWarning)
	t.CPUCritical = parseEnvFloat("FORECASTER_CPU_CRITICAL", t.CPUCritical)
	t.MemoryWarning = parseEnvFloat("FORECASTER_MEMORY_WARNING", t.MemoryWarning)
	t.MemoryCritical = parseEnvFloat("FORECASTER_MEMORY_CRITICAL", t.MemoryCritical)
	t.GPUWarning = parseEnvFloat("FORECASTER_GPU_WARNING", t.GPUWarning)
	t.GPUCritical = parseEnvFloat("FORECASTER_GPU_CRITICAL", t.GPUCritical)
	t.StorageWarning = parseEnvFloat("FORECASTER_STORAGE_WARNING", t.StorageWarning)
	t.StorageCritical = parseEnvFloat("FORECASTER_STORAGE_CRITICAL", t.StorageCritical)
	return t
}
