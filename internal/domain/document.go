package domain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type Doc map[string]any

func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func S(d Doc, k string) string {
	v := d[k]
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func N(d Doc, k string) float64 {
	switch n := d[k].(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	case uint:
		return float64(n)
	case uint32:
		return float64(n)
	case int32:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}
func B(d Doc, k string) bool { v, _ := d[k].(bool); return v }
func M(d Doc, k string) Doc {
	switch m := d[k].(type) {
	case map[string]any:
		return m
	case Doc:
		return m
	}
	return Doc{}
}
func A(d Doc, k string) []Doc {
	out := []Doc{}
	switch a := d[k].(type) {
	case []any:
		for _, v := range a {
			switch m := v.(type) {
			case map[string]any:
				out = append(out, m)
			case Doc:
				out = append(out, m)
			}
		}
	case []Doc:
		return a
	}
	return out
}
func Clone(d Doc) Doc { b, _ := json.Marshal(d); out := Doc{}; _ = json.Unmarshal(b, &out); return out }
