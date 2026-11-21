package filesystem

import (
	"errors"
	"strings"
	"testing"

	dContract "github.com/vendelev/sre-metrics/pkg/fileinbox/internal/domain/contract"
)

func TestReadLimitedDetectsExtraByte(t *testing.T) {
	t.Parallel()
	contents, err := readLimited(strings.NewReader("123"), 2)
	if contents != nil || !errors.Is(err, dContract.ErrFileTooLarge) {
		t.Errorf("лишний байт не отклонён: %q, %v", contents, err)
	}
}
