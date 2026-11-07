package store

import (
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

func mysqlTime(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// Time keeps lexical comparisons and UTC day partition boundaries consistent.
// RFC3339Nano trims fractional zeroes, which does not preserve lexical order.
// All MySQL SQL timestamp arguments, including rollup buckets, use this helper.
func (s *Store) Time(at time.Time) string {
	if s.Driver == "mysql" {
		return mysqlTime(at)
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func (s *Store) normalizeTimes(d domain.Doc) {
	if s.Driver != "mysql" {
		return
	}
	for key, value := range d {
		if !strings.HasSuffix(key, "_at") && !strings.HasSuffix(key, "_until") {
			continue
		}
		if text, ok := value.(string); ok {
			if at, e := time.Parse(time.RFC3339Nano, text); e == nil {
				d[key] = mysqlTime(at)
			}
		}
	}
}
