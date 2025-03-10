package util

import (
	"context"
	"fmt"
	"strconv"
)

const LogAttributeValueLengthLimit = 2 << 15
const LogAttributeValueWarningLengthLimit = 2 << 9

func FormatLogAttributes(ctx context.Context, k string, v interface{}) map[string]string {
	if vStr, ok := v.(string); ok {
		if len(vStr) > LogAttributeValueLengthLimit {
			vStr = vStr[:LogAttributeValueLengthLimit] + "..."
		}
		return map[string]string{k: vStr}
	}
	if vInt, ok := v.(int64); ok {
		return map[string]string{k: strconv.FormatInt(vInt, 10)}
	}
	if vFlt, ok := v.(float64); ok {
		return map[string]string{k: strconv.FormatFloat(vFlt, 'f', -1, 64)}
	}
	if vMap, ok := v.(map[string]interface{}); ok {
		m := make(map[string]string)
		for mapKey, mapV := range vMap {
			for k2, v2 := range FormatLogAttributes(ctx, mapKey, mapV) {
				m[fmt.Sprintf("%s.%s", k, k2)] = v2
			}
		}
		return m
	}
	return nil
}
