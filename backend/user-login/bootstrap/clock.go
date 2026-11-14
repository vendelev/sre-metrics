package bootstrap

import (
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/clock"
)

// Clock — часы доменных решений модуля: время события и обслуживания воркер
// берёт один раз в начале операции. Реализация передаётся при сборке, поэтому
// приложение и тесты собирают модуль с управляемым временем, не меняя код
// внутренних слоёв.
type Clock = contract.Clock

// RealTime — рабочие часы модуля.
type RealTime = clock.RealTime
