package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type BucketMetrics struct {
	TotalRequests int64 `json:"total_requests"`
	CacheHits     int64 `json:"cache_hits"`
	OriginHits    int64 `json:"origin_hits"`
	TotalSuccess  int64 `json:"total_success"`
	Total4xx      int64 `json:"total_4xx"`
	Total5xx      int64 `json:"total_5xx"`
	TotalBytes    int64 `json:"total_bytes"`
	LastHit       int64 `json:"last_hit"`
}

var globalMetrics sync.Map

func recordHit(bucket string, statusCode int, bytes int64, isCacheHit bool, country string, opType string) {
	if country == "" {
		country = "XX" // Unknown — avoids false US attribution from internal/direct requests
	}

	val, ok := globalMetrics.Load(bucket)
	var m *BucketMetrics
	if !ok {
		m = &BucketMetrics{}
		val, _ = globalMetrics.LoadOrStore(bucket, m)
	}
	m = val.(*BucketMetrics)

	atomic.AddInt64(&m.TotalRequests, 1)
	if isCacheHit {
		atomic.AddInt64(&m.CacheHits, 1)
	} else {
		atomic.AddInt64(&m.OriginHits, 1)
	}
	if statusCode >= 200 && statusCode < 300 {
		atomic.AddInt64(&m.TotalSuccess, 1)
	} else if statusCode >= 400 && statusCode < 500 {
		atomic.AddInt64(&m.Total4xx, 1)
	} else if statusCode >= 500 {
		atomic.AddInt64(&m.Total5xx, 1)
	}

	if bytes > 0 {
		atomic.AddInt64(&m.TotalBytes, bytes)
	}
	atomic.StoreInt64(&m.LastHit, time.Now().Unix())

	if scyllaSession != nil {
		hour := time.Now().Truncate(time.Hour)
		go func() {
			isRead := 0
			isWrite := 0
			if opType == "write" {
				isWrite = 1
			} else {
				isRead = 1
			}
			scyllaSession.Query(`UPDATE cosmos.s3_analytics_hourly 
				SET total_requests = total_requests + 1,
				    total_bytes = total_bytes + ?,
				    cache_hits = cache_hits + ?,
				    origin_hits = origin_hits + ?,
				    total_success = total_success + ?,
				    total_errors = total_errors + ?,
				    total_reads = total_reads + ?,
				    total_writes = total_writes + ?
				WHERE bucket_name=? AND hour=? AND country=?`,
				bytes,
				btoi(isCacheHit),
				btoi(!isCacheHit),
				btoi(statusCode >= 200 && statusCode < 300),
				btoi(statusCode >= 400),
				isRead,
				isWrite,
				bucket, hour, country).Exec()
		}()
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func getMetrics(bucket string) BucketMetrics {
	val, ok := globalMetrics.Load(bucket)
	if !ok {
		return BucketMetrics{}
	}
	m := val.(*BucketMetrics)
	return BucketMetrics{
		TotalRequests: atomic.LoadInt64(&m.TotalRequests),
		CacheHits:     atomic.LoadInt64(&m.CacheHits),
		OriginHits:    atomic.LoadInt64(&m.OriginHits),
		TotalSuccess:  atomic.LoadInt64(&m.TotalSuccess),
		Total4xx:      atomic.LoadInt64(&m.Total4xx),
		Total5xx:      atomic.LoadInt64(&m.Total5xx),
		TotalBytes:    atomic.LoadInt64(&m.TotalBytes),
		LastHit:       atomic.LoadInt64(&m.LastHit),
	}
}

func init() {
	go func() {
		time.Sleep(5 * time.Second)
		if scyllaSession == nil {
			return
		}

		iter := scyllaSession.Query(`SELECT bucket_name, total_requests, cache_hits, origin_hits, total_success, total_4xx, total_5xx, total_bytes, last_hit FROM cosmos.s3_analytics`).Iter()
		var b string
		var tr, ch, oh, ts, t4, t5, tb, lh int64
		for iter.Scan(&b, &tr, &ch, &oh, &ts, &t4, &t5, &tb, &lh) {
			m := &BucketMetrics{
				TotalRequests: tr, CacheHits: ch, OriginHits: oh,
				TotalSuccess: ts, Total4xx: t4, Total5xx: t5,
				TotalBytes: tb, LastHit: lh,
			}
			globalMetrics.Store(b, m)
		}
		iter.Close()

		for {
			time.Sleep(5 * time.Second)
			globalMetrics.Range(func(key, value any) bool {
				bucket := key.(string)
				m := value.(*BucketMetrics)

				scyllaSession.Query(`UPDATE cosmos.s3_analytics SET total_requests=?, cache_hits=?, origin_hits=?, total_success=?, total_4xx=?, total_5xx=?, total_bytes=?, last_hit=? WHERE bucket_name=?`,
					atomic.LoadInt64(&m.TotalRequests), atomic.LoadInt64(&m.CacheHits), atomic.LoadInt64(&m.OriginHits),
					atomic.LoadInt64(&m.TotalSuccess), atomic.LoadInt64(&m.Total4xx), atomic.LoadInt64(&m.Total5xx),
					atomic.LoadInt64(&m.TotalBytes), atomic.LoadInt64(&m.LastHit), bucket).Exec()
				return true
			})
		}
	}()
}
