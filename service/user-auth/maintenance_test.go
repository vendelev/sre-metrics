package userauth

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestMaintainStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		done := make(chan error, 1)
		go func() { done <- maintain(ctx, time.Second, func() error { calls++; return nil }) }()
		synctest.Wait()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		<-timer.C
		synctest.Wait()
		if calls < 2 {
			t.Fatalf("периодическое обслуживание не выполняется: %d вызовов", calls)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		before := calls
		timer.Reset(3 * time.Second)
		<-timer.C
		synctest.Wait()
		if calls != before {
			t.Fatal("после завершения задачи появились новые вызовы")
		}
	})
}
