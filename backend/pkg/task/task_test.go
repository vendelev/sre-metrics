package task_test

import (
	"errors"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/pkg/task"
)

func TestTaskPublishesResultOnce(t *testing.T) {
	t.Parallel()
	cause := errors.New("синтетический сбой задачи")
	bgTask := task.New()
	bgTask.Complete(cause)
	bgTask.Complete(nil)

	select {
	case <-bgTask.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались закрытия Done")
	}
	if !errors.Is(bgTask.Err(), cause) {
		t.Fatalf("результат задачи потерян или заменен: %v", bgTask.Err())
	}
}
