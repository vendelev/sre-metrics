package fileinbox

import (
	"context"
	"syscall"

	"go.opentelemetry.io/otel/metric"
)

func newVolumeGauge(meter metric.Meter) (metric.Float64Gauge, error) {
	return meter.Float64Gauge(
		volumeUsageName, metric.WithUnit("1"),
		metric.WithDescription("Доля занятых блоков всей файловой системы тома, а не только каталога входа"),
	)
}

func (s *scanner) observeVolume(ctx context.Context) {
	// Statfs видит общий tmpfs, включая промежуточные файлы и соседние каталоги.
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.Dir, &stat); err != nil {
		s.diagnostics.add(ctx, ioError, 1)
		s.logger.WarnContext(ctx, "не удалось измерить заполнение тома файлового входа")
		return
	}
	if stat.Blocks == 0 {
		return
	}
	used := stat.Blocks - min(stat.Blocks, stat.Bfree)
	s.diagnostics.volumeUsage.Record(ctx, float64(used)/float64(stat.Blocks))
}
