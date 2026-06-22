/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import "strings"

// extractResultResourceID는 status.result에서 오퍼레이션 리소스 ID를 추출한다.
// type=...;id=... 형식과 마이그레이션용 순수 migration ID 문자열을 모두 지원한다.
func extractResultResourceID(result string) (id string, ok bool) {
	result = strings.TrimSpace(result)
	if result == "" {
		return "", false
	}
	if strings.Contains(result, "id=") {
		for _, part := range strings.Split(result, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "id=") {
				v := strings.TrimPrefix(part, "id=")
				if v != "" {
					return v, true
				}
			}
		}
		return "", false
	}
	if !strings.Contains(result, "=") {
		return result, true
	}
	return "", false
}
