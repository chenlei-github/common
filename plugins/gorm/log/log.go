package log

import (
	"context"
	"fmt"
	"github.com/go-cinch/common/log"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"time"
)

const HiddenSql = "gorm.hidden.sql"

type gormLogger struct {
	ops                                          Options
	level                                        logger.LogLevel
	normalStr, normalErrStr, slowStr, slowErrStr string
}

func New(options ...func(*Options)) logger.Interface {
	ops := getOptionsOrSetDefault(nil)
	for _, f := range options {
		f(ops)
	}

	var (
		normalStr    = "[%.3fms] [rows:%v] %s"
		slowStr      = "[%.3fms(slow)] [rows:%v] %s"
		normalErrStr = "%s\n[%.3fms] [rows:%v] %s"
		slowErrStr   = "%s\n[%.3fms(slow)] [rows:%v] %s"
	)

	if ops.colorful {
		normalStr = logger.Green + "[%.3fms] " + logger.Reset + logger.BlueBold + "[rows:%v]" + logger.Reset + " %s"
		slowStr = logger.Yellow + "[%.3fms(slow)] " + logger.Reset + logger.BlueBold + "[rows:%v]" + logger.Reset + " %s"
		normalErrStr = logger.RedBold + "%s\n" + logger.Reset + logger.Green + "[%.3fms] " + logger.Reset + logger.BlueBold + "[rows:%v]" + logger.Reset + " %s"
		slowErrStr = logger.RedBold + "%s\n" + logger.Reset + logger.Yellow + "[%.3fms(slow)] " + logger.Reset + logger.BlueBold + "[rows:%v]" + logger.Reset + " %s"
	}

	l := gormLogger{
		ops:          *ops,
		level:        levelToGorm(ops.level),
		normalStr:    normalStr,
		slowStr:      slowStr,
		normalErrStr: normalErrStr,
		slowErrStr:   slowErrStr,
	}
	return &l
}

func (l *gormLogger) LogMode(level logger.LogLevel) logger.Interface {
	newLogger := *l
	newLogger.level = level
	return &newLogger
}

func (l gormLogger) Info(ctx context.Context, format string, args ...interface{}) {
	if l.level >= logger.Info {
		log.WithContext(ctx).Info(fmt.Sprintf(format, args...))
	}
}

func (l gormLogger) Warn(ctx context.Context, format string, args ...interface{}) {
	if l.level >= logger.Warn {
		log.WithContext(ctx).Warn(fmt.Sprintf(format, args...))
	}
}

func (l gormLogger) Error(ctx context.Context, format string, args ...interface{}) {
	if l.level >= logger.Error {
		log.WithContext(ctx).Error(fmt.Sprintf(format, args...))
	}
}

func (l gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level > logger.Silent {
		elapsed := time.Since(begin)
		elapsedF := float64(elapsed.Nanoseconds()) / 1e6
		sql, rows := fc()
		row := "-"
		if rows > -1 {
			row = fmt.Sprintf("%d", rows)
		}
		hiddenSql := false
		if v, ok := ctx.Value(HiddenSql).(bool); ok {
			hiddenSql = v
		}
		if hiddenSql {
			sql = "(sql is hidden)"
		}
		switch {
		case l.level >= logger.Error && err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
			if l.ops.slow > 0 && elapsed > l.ops.slow {
				l.Warn(ctx, l.slowErrStr, err, elapsedF, row, sql)
			} else {
				l.Error(ctx, l.normalErrStr, err, elapsedF, row, sql)
			}
		case l.level >= logger.Warn && l.ops.slow > 0 && elapsed > l.ops.slow:
			l.Warn(ctx, l.slowStr, elapsedF, row, sql)
		case l.level == logger.Info:
			l.Info(ctx, l.normalStr, elapsedF, row, sql)
		}
	}
}

func levelToGorm(l log.Level) logger.LogLevel {
	switch l {
	case log.FatalLevel, log.ErrorLevel:
		return logger.Error
	case log.WarnLevel:
		return logger.Warn
	case log.InfoLevel, log.DebugLevel, log.TraceLevel:
		return logger.Info
	default:
		return logger.Silent
	}
}
