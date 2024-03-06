package retry_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/stretchr/testify/assert"
)

// RetriableError is a custom error that contains a positive duration for the next retry
type RetriableError struct {
	Err        error
	RetryAfter time.Duration
}

// Error returns error message and a Retry-After duration
func (e *RetriableError) Error() string {
	return fmt.Sprintf("%s (retry after %v)", e.Err.Error(), e.RetryAfter)
}

var _ error = (*RetriableError)(nil)

// TestCustomRetryFunction shows how to use a custom retry function
func TestCustomRetryFunction(t *testing.T) {
	// 首先定义 server 的行为
	attempts := 5 // server succeeds after 5 attempts
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts > 0 {
			// 前 5 次会返回 HTTP 429 通知 client 1 秒后重试，第 6 次返回 HTTP 200
			// inform the client to retry after one second using standard
			// HTTP 429 status code with Retry-After header in seconds
			w.Header().Add("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("Server limit reached"))
			attempts--
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))
	defer ts.Close()

	var body []byte

	// 其次定义 client 的行为，通过 retry.Do() 封装 http 调用
	err := retry.Do(
		// 执行的函数会返回 error
		func() error {
			resp, err := http.Get(ts.URL)

			if err == nil {
				defer func() {
					if err := resp.Body.Close(); err != nil {
						panic(err)
					}
				}()
				body, err = ioutil.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
					if resp.StatusCode == http.StatusTooManyRequests {
						// check Retry-After header if it contains seconds to wait for the next retry
						// 解析 Retry-After header，其中包含了下次重试的秒数
						if retryAfter, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); e == nil {
							// the server returns 0 to inform that the operation cannot be retried
							if retryAfter <= 0 {
								return retry.Unrecoverable(err)
							}
							return &RetriableError{
								Err:        err,
								RetryAfter: time.Duration(retryAfter) * time.Second, // 使用从 server 返回的 Retry-After header 中解析的秒数，作为下次重试的间隔
							}
						}
						// A real implementation should also try to http.Parse the retryAfter response header
						// to conform with HTTP specification. Herein we know here that we return only seconds.
					}
				}
			}

			return err
		},

		// 执行完 func() error 后会得到 err，会执行 retry.DelayType(err)
		// 如果 err 是 *RetriableError 类型，会返回 e.RetryAfter 的 time.Duration
		// 否则如果 err 不是 *RetriableError 类型，会返回默认的 retry.BackOffDelay(n, err, config)
		retry.DelayType(func(n uint, err error, config *retry.Config) time.Duration {
			fmt.Println("Server fails with: " + err.Error())
			if retriable, ok := err.(*RetriableError); ok {
				fmt.Printf("Client follows server recommendation to retry after %v\n", retriable.RetryAfter)
				return retriable.RetryAfter
			}
			// apply a default exponential back off strategy
			return retry.BackOffDelay(n, err, config)
		}),
	)

	fmt.Println("Server responds with: " + string(body))

	assert.NoError(t, err)
	assert.Equal(t, "hello", string(body))
}
