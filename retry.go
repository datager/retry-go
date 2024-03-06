/*
Simple library for retry mechanism

slightly inspired by [Try::Tiny::Retry](https://metacpan.org/pod/Try::Tiny::Retry)

# SYNOPSIS

http get with retry:

	url := "http://example.com"
	var body []byte

	err := retry.Do(
		func() error {
			resp, err := http.Get(url)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			return nil
		},
	)
	if err != nil {
		// handle error
	}

	fmt.Println(string(body))

http get with retry with data:

	url := "http://example.com"

	body, err := retry.DoWithData(
		func() ([]byte, error) {
			resp, err := http.Get(url)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}

			return body, nil
		},
	)
	if err != nil {
		// handle error
	}

	fmt.Println(string(body))

[next examples](https://github.com/avast/retry-go/tree/master/examples)

# SEE ALSO

* [giantswarm/retry-go](https://github.com/giantswarm/retry-go) - slightly complicated interface.

* [sethgrid/pester](https://github.com/sethgrid/pester) - only http retry for http calls with retries and backoff

* [cenkalti/backoff](https://github.com/cenkalti/backoff) - Go port of the exponential backoff algorithm from Google's HTTP Client Library for Java. Really complicated interface.

* [rafaeljesus/retry-go](https://github.com/rafaeljesus/retry-go) - looks good, slightly similar as this package, don't have 'simple' `Retry` method

* [matryer/try](https://github.com/matryer/try) - very popular package, nonintuitive interface (for me)

# BREAKING CHANGES

* 4.0.0
  - infinity retry is possible by set `Attempts(0)` by PR [#49](https://github.com/avast/retry-go/pull/49)

* 3.0.0
  - `DelayTypeFunc` accepts a new parameter `err` - this breaking change affects only your custom Delay Functions. This change allow [make delay functions based on error](examples/delay_based_on_error_test.go).

* 1.0.2 -> 2.0.0
  - argument of `retry.Delay` is final delay (no multiplication by `retry.Units` anymore)
  - function `retry.Units` are removed
  - [more about this breaking change](https://github.com/avast/retry-go/issues/7)

* 0.3.0 -> 1.0.0
  - `retry.Retry` function are changed to `retry.Do` function
  - `retry.RetryCustom` (OnRetry) and `retry.RetryCustomWithOpts` functions are now implement via functions produces Options (aka `retry.OnRetry`)
*/
package retry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Function signature of retryable function
type RetryableFunc func() error

// Function signature of retryable function with data
type RetryableFuncWithData[T any] func() (T, error)

// Default timer is a wrapper around time.After
type timerImpl struct{}

func (t *timerImpl) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

func Do(retryableFunc RetryableFunc, opts ...Option) error {
	retryableFuncWithData := func() (any, error) {
		// 执行 retryableFunc() 会返回 error
		// 再封装一个 any, 连同 error 一起返回
		return nil, retryableFunc()
	}

	_, err := DoWithData(retryableFuncWithData, opts...)
	return err
}

func DoWithData[T any](retryableFunc RetryableFuncWithData[T], opts ...Option) (T, error) {
	var n uint
	var emptyT T

	// default
	config := newDefaultRetryConfig()

	// apply opts
	for _, opt := range opts {
		opt(config)
	}

	if err := config.context.Err(); err != nil {
		return emptyT, err
	}

	// Setting attempts to 0 means we'll retry until we succeed
	var lastErr error
	if config.attempts == 0 {
		for {
			t, err := retryableFunc()
			if err == nil {
				return t, nil
			}

			if !IsRecoverable(err) {
				return emptyT, err
			}

			if !config.retryIf(err) {
				return emptyT, err
			}

			lastErr = err

			n++
			config.onRetry(n, err)
			select {
			case <-config.timer.After(delay(config, n, err)):
			case <-config.context.Done():
				if config.wrapContextErrorWithLastError {
					return emptyT, Error{config.context.Err(), lastErr}
				}
				return emptyT, config.context.Err()
			}
		}
	}

	// 主流程开始
	errorLog := Error{} // 这是一个数组, 记录了所有的错误

	// 因为后续会修改 attempts 值, 所以这里先拷贝一份, 后续使用拷贝的那一份
	attemptsForError := make(map[error]uint, len(config.attemptsForError))
	for err, attempts := range config.attemptsForError {
		attemptsForError[err] = attempts
	}

	shouldRetry := true // 当超出重试次数时, 会退出循环
	for shouldRetry {
		// 执行用户传入的主流程函数, 我们要重试的就是他
		t, err := retryableFunc()
		// 如果执行成功了, 直接返回, 不需要再重试了
		if err == nil {
			return t, nil
		}

		// 追加 error
		errorLog = append(errorLog, unpackUnrecoverable(err))

		// 用户可以自定义回调函数, 即根据返回的 err 判断是否需要重试
		if !config.retryIf(err) {
			break
		}

		// 当重试时, 需要执行的回调函数, 用户可以自定义
		config.onRetry(n, err)

		// 用户可以设置某种 err 需要重试几次. 此处会判断返回的 err 并减少需要重试的次数
		for errToCheck, attempts := range attemptsForError {
			if errors.Is(err, errToCheck) {
				attempts--
				attemptsForError[errToCheck] = attempts
				shouldRetry = shouldRetry && attempts > 0
			}
		}

		// 既然最后一次 retryableFunc() 已经执行完了, 那就不需要再等待了
		// if this is last attempt - don't wait
		if n == config.attempts-1 {
			break
		}

		select {
		case <-config.timer.After(delay(config, n, err)): // 等待一段时间后再重试
		case <-config.context.Done(): // 如果用户把 context Done() 了, 则退出即可. 通常原因是用户主动 ctx.Cancel() 或者 ctx.Timeout() 自己到达了
			if config.lastErrorOnly {
				return emptyT, config.context.Err()
			}

			return emptyT, append(errorLog, config.context.Err())
		}

		n++
		shouldRetry = shouldRetry && n < config.attempts // 总的 attempts 次数也会控制是否需要重试
	}

	if config.lastErrorOnly {
		return emptyT, errorLog.Unwrap() // 这个 errorLog 其实是一个数组, Unwrap() 其实就是返回数组的最后一项
	}
	return emptyT, errorLog
}

func newDefaultRetryConfig() *Config {
	return &Config{
		attempts:         uint(10),
		attemptsForError: make(map[error]uint),
		delay:            100 * time.Millisecond,
		maxJitter:        100 * time.Millisecond,
		onRetry:          func(n uint, err error) {},
		retryIf:          IsRecoverable, // 通过自定义类型实现
		delayType:        CombineDelay(BackOffDelay, RandomDelay),
		lastErrorOnly:    false,
		context:          context.Background(),
		timer:            &timerImpl{},
	}
}

// Error type represents list of errors in retry
type Error []error

// Error method return string representation of Error
// It is an implementation of error interface
func (e Error) Error() string {
	logWithNumber := make([]string, len(e))
	for i, l := range e {
		if l != nil {
			logWithNumber[i] = fmt.Sprintf("#%d: %s", i+1, l.Error())
		}
	}

	return fmt.Sprintf("All attempts fail:\n%s", strings.Join(logWithNumber, "\n"))
}

func (e Error) Is(target error) bool {
	for _, v := range e {
		if errors.Is(v, target) {
			return true
		}
	}
	return false
}

func (e Error) As(target interface{}) bool {
	for _, v := range e {
		if errors.As(v, target) {
			return true
		}
	}
	return false
}

/*
Unwrap the last error for compatibility with `errors.Unwrap()`.
When you need to unwrap all errors, you should use `WrappedErrors()` instead.

	err := Do(
		func() error {
			return errors.New("original error")
		},
		Attempts(1),
	)

	fmt.Println(errors.Unwrap(err)) # "original error" is printed

Added in version 4.2.0.
*/
func (e Error) Unwrap() error {
	return e[len(e)-1]
}

// WrappedErrors returns the list of errors that this Error is wrapping.
// It is an implementation of the `errwrap.Wrapper` interface
// in package [errwrap](https://github.com/hashicorp/errwrap) so that
// `retry.Error` can be used with that library.
func (e Error) WrappedErrors() []error {
	return e
}

type unrecoverableError struct {
	error
}

func (e unrecoverableError) Error() string {
	if e.error == nil {
		return "unrecoverable error"
	}
	return e.error.Error()
}

func (e unrecoverableError) Unwrap() error {
	return e.error
}

// Unrecoverable wraps an error in `unrecoverableError` struct
func Unrecoverable(err error) error {
	return unrecoverableError{err}
}

// IsRecoverable checks if error is an instance of `unrecoverableError`
func IsRecoverable(err error) bool {
	return !errors.Is(err, unrecoverableError{})
}

// Adds support for errors.Is usage on unrecoverableError
func (unrecoverableError) Is(err error) bool {
	_, isUnrecoverable := err.(unrecoverableError)
	return isUnrecoverable
}

func unpackUnrecoverable(err error) error {
	if unrecoverable, isUnrecoverable := err.(unrecoverableError); isUnrecoverable {
		return unrecoverable.error
	}

	return err
}

func delay(config *Config, n uint, err error) time.Duration {
	delayTime := config.delayType(n, err, config)
	if config.maxDelay > 0 && delayTime > config.maxDelay {
		delayTime = config.maxDelay
	}

	return delayTime
}
