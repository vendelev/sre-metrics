package config

import (
	"fmt"
	"strings"
	"time"
)

// Inbox задаёт каталог, период сканирования и лимиты файлового входа модуля.
type Inbox struct {
	Dir             string        `env:"USER_LOGIN_INBOX_DIR"`
	ScanInterval    time.Duration `env:"USER_LOGIN_INBOX_SCAN_INTERVAL"`
	MaxFilesPerScan int           `env:"USER_LOGIN_INBOX_MAX_FILES_PER_SCAN"`
	MaxFileBytes    int64         `env:"USER_LOGIN_INBOX_MAX_FILE_BYTES"`
	MaxAttempts     int           `env:"USER_LOGIN_INBOX_MAX_ATTEMPTS"`
}

func defaultInbox() Inbox {
	return Inbox{
		Dir:             "/mnt/metrics/user-login",
		ScanInterval:    time.Second,
		MaxFilesPerScan: 256,
		MaxFileBytes:    16 * 1024,
		MaxAttempts:     3,
	}
}

func (i Inbox) validatePositive() error {
	return validatePositive(
		numericSetting{name: "ScanInterval", value: int64(i.ScanInterval)},
		numericSetting{name: "MaxFilesPerScan", value: int64(i.MaxFilesPerScan)},
		numericSetting{name: "MaxFileBytes", value: i.MaxFileBytes},
		numericSetting{name: "MaxAttempts", value: int64(i.MaxAttempts)},
	)
}

func (i Inbox) validateDir() error {
	if strings.TrimSpace(i.Dir) == "" {
		return fmt.Errorf("%s: требуется непустой путь к каталогу", envName("Dir"))
	}
	return nil
}
