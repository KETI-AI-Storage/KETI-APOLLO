/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package config는 Orchestration Policy Engine의 env/ConfigMap 기반 런타임 설정 파싱을 담당한다.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// PolicyEngineRuntime은 Orchestration Policy Engine이 env에서 읽는 운영 설정이다.
// ConfigMap을 envFrom으로 주입하면 값을 외부에서 조정할 수 있다.
type PolicyEngineRuntime struct {
	PolicyCheckInterval         time.Duration
	PolicyTTL                   time.Duration
	ForecasterHTTPTimeout       time.Duration
	OperatorHTTPTimeout         time.Duration
	PolicyAgentHTTPTimeout      time.Duration
	PolicyAgentRequeueAfter     time.Duration
	OperatorHTTPMaxRetries      int
	OperatorHTTPRetryBackoff    time.Duration
	KubeClientQPS               float32
	KubeClientBurst             int
	MaxConcurrentReconciles     int
	DisableDuplicatePolicyCheck bool

	MigrationAPITimeoutSec int

	MigrationMaxConsecutive404        int
	MigrationMaxConsecutivePollErrors int

	SyncMaxConsecutive404       int
	SyncMaxConsecutiveGETErrors int

	// ProvisioningStrictReadyOnly가 true이면 프로비저닝 Completed는 Operator status=ready일 때만 허용한다.
	ProvisioningStrictReadyOnly bool

	// TerminatedPolicyGCEnabled가 true이면 종료(Completed/Failed/Rejected) OrchestrationPolicy를 주기적으로 정리한다.
	TerminatedPolicyGCEnabled bool
	// TerminatedPolicyGCInterval은 종료 정책 정리 주기다.
	TerminatedPolicyGCInterval time.Duration
	// TerminatedPolicyTTL은 종료 시각 이후 CR을 보존하는 시간(초과 시 삭제)이다.
	TerminatedPolicyTTL time.Duration
	// TerminatedPolicyGCBatchMax는 한 정리 주기당 삭제 건수 상한이다(etcd 부하 방지).
	TerminatedPolicyGCBatchMax int
}

func parsePositiveIntEnv(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultVal
	}
	return n
}

func parseDurationSecondsEnv(key string, defaultSec int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(defaultSec) * time.Second
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec <= 0 {
		return time.Duration(defaultSec) * time.Second
	}
	return time.Duration(sec) * time.Second
}

func parseNonNegativeIntEnv(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}

func parsePositiveFloatEnv(key string, defaultVal float32) float32 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 32)
	if err != nil || f <= 0 {
		return defaultVal
	}
	return float32(f)
}

// parseBoolEnv는 비어 있거나 파싱 불가하면 defaultVal을 사용한다.
// "true"/"false"(대소문자 무시)만 인식하며, 기본값이 true인 토글에 사용한다.
func parseBoolEnv(key string, defaultVal bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	switch {
	case strings.EqualFold(v, "true"):
		return true
	case strings.EqualFold(v, "false"):
		return false
	default:
		return defaultVal
	}
}

// LoadPolicyEngineRuntime은 환경변수에서 PolicyEngineRuntime을 구성한다.
// 비어 있거나 잘못된 값은 안전한 기본값으로 대체된다.
func LoadPolicyEngineRuntime() PolicyEngineRuntime {
	return PolicyEngineRuntime{
		PolicyCheckInterval:         parseDurationSecondsEnv("POLICY_GENERATOR_CHECK_INTERVAL_SECONDS", 60),
		PolicyTTL:                   parseDurationSecondsEnv("POLICY_GENERATOR_TTL_SECONDS", 600),
		ForecasterHTTPTimeout:       parseDurationSecondsEnv("FORECASTER_HTTP_TIMEOUT_SECONDS", 10),
		OperatorHTTPTimeout:         parseDurationSecondsEnv("OPERATOR_HTTP_TIMEOUT_SECONDS", 30),
		PolicyAgentHTTPTimeout:      parseDurationSecondsEnv("POLICY_AGENT_HTTP_TIMEOUT_SECONDS", 5),
		PolicyAgentRequeueAfter:     parseDurationSecondsEnv("POLICY_AGENT_REQUEUE_AFTER_SECONDS", 30),
		OperatorHTTPMaxRetries:      parseNonNegativeIntEnv("OPERATOR_HTTP_MAX_RETRIES", 2),
		OperatorHTTPRetryBackoff:    time.Duration(parsePositiveIntEnv("OPERATOR_HTTP_RETRY_BACKOFF_MS", 300)) * time.Millisecond,
		KubeClientQPS:               parsePositiveFloatEnv("KUBE_CLIENT_QPS", 20),
		KubeClientBurst:             parsePositiveIntEnv("KUBE_CLIENT_BURST", 40),
		MaxConcurrentReconciles:     parsePositiveIntEnv("POLICY_MAX_CONCURRENT_RECONCILES", 1),
		DisableDuplicatePolicyCheck: strings.EqualFold(os.Getenv("POLICY_GENERATOR_DISABLE_DUPLICATE_CHECK"), "true"),

		MigrationAPITimeoutSec: parsePositiveIntEnv("MIGRATION_API_TIMEOUT_SECONDS", 600),

		MigrationMaxConsecutive404:        parsePositiveIntEnv("POLICY_MIGRATION_MAX_CONSECUTIVE_404", 5),
		MigrationMaxConsecutivePollErrors: parsePositiveIntEnv("POLICY_MIGRATION_MAX_CONSECUTIVE_POLL_ERRORS", 10),

		SyncMaxConsecutive404:       parsePositiveIntEnv("POLICY_SYNC_MAX_CONSECUTIVE_404", 5),
		SyncMaxConsecutiveGETErrors: parsePositiveIntEnv("POLICY_SYNC_MAX_CONSECUTIVE_GET_ERRORS", 15),

		ProvisioningStrictReadyOnly: strings.EqualFold(os.Getenv("POLICY_PROVISIONING_STRICT_READY_ONLY"), "true"),

		TerminatedPolicyGCEnabled:  parseBoolEnv("POLICY_GC_ENABLED", true),
		TerminatedPolicyGCInterval: parseDurationSecondsEnv("POLICY_GC_INTERVAL_SECONDS", 300),
		TerminatedPolicyTTL:        parseDurationSecondsEnv("POLICY_GC_TTL_SECONDS", 600),
		TerminatedPolicyGCBatchMax: parsePositiveIntEnv("POLICY_GC_BATCH_MAX", 200),
	}
}
