package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/go-cinch/common/lock"
	"github.com/go-cinch/common/log"
	"github.com/go-redis/redis/v8"
	"github.com/golang-module/carbon/v2"
	"github.com/google/uuid"
	"github.com/gorhill/cronexpr"
	"github.com/hibiken/asynq"
	"github.com/pkg/errors"
	"net/http"
	"strings"
	"time"
)

type Worker struct {
	ops       Options
	redis     redis.UniversalClient
	redisOpt  asynq.RedisConnOpt
	lock      *lock.NxLock
	client    *asynq.Client
	inspector *asynq.Inspector
	Error     error
}

type periodTask struct {
	Expr      string `json:"expr"` // cron expr github.com/robfig/cron/v3
	Name      string `json:"group"`
	Uid       string `json:"uid"`
	Payload   string `json:"payload"`
	Next      int64  `json:"next"`      // next schedule unix timestamp
	Processed int64  `json:"processed"` // run times
	MaxRetry  int    `json:"maxRetry"`
	Timeout   int    `json:"timeout"`
}

func (p periodTask) String() (str string) {
	bs, _ := json.Marshal(p)
	str = string(bs)
	return
}

func (p *periodTask) FromString(str string) {
	json.Unmarshal([]byte(str), p)
	return
}

type periodTaskHandler struct {
	tk Worker
}

type Payload struct {
	Category string `json:"category"`
	Uid      string `json:"uid"`
	Payload  string `json:"payload"`
}

func (p Payload) String() (str string) {
	bs, _ := json.Marshal(p)
	str = string(bs)
	return
}

func (p periodTaskHandler) ProcessTask(ctx context.Context, t *asynq.Task) (err error) {
	uid := uuid.NewString()
	payload := Payload{
		Category: t.Type(),
		Uid:      t.ResultWriter().TaskID(),
		Payload:  string(t.Payload()),
	}
	defer func() {
		if err != nil {
			log.
				WithError(err).
				WithFields(log.Fields{
					"task": payload,
					"uuid": uid,
				}).
				Error("run task failed")
		}
	}()
	if p.tk.ops.handler != nil {
		err = p.tk.ops.handler(ctx, payload)
	} else if p.tk.ops.callback != "" {
		err = p.httpCallback(ctx, payload)
	} else {
		log.
			WithContext(ctx).
			WithFields(log.Fields{
				"task": payload,
				"uuid": uid,
			}).
			Info("no task handler")
	}
	// save processed count
	p.tk.processed(payload.Uid)
	return
}

func (p periodTaskHandler) httpCallback(ctx context.Context, payload Payload) (err error) {
	client := &http.Client{}
	body := payload.String()
	var r *http.Request
	r, _ = http.NewRequestWithContext(ctx, http.MethodPost, p.tk.ops.callback, bytes.NewReader([]byte(body)))
	r.Header.Add("Content-Type", "application/json")
	var res *http.Response
	res, err = client.Do(r)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		err = ErrHttpCallbackInvalidStatusCode
	}
	return
}

// New is create a task worker, implemented by asynq: https://github.com/hibiken/asynq
func New(options ...func(*Options)) (tk *Worker) {
	ops := getOptionsOrSetDefault(nil)
	for _, f := range options {
		f(ops)
	}
	tk = &Worker{}
	if ops.redisUri == "" {
		tk.Error = errors.WithStack(ErrRedisNil)
		return
	}
	rs, err := asynq.ParseRedisURI(ops.redisUri)
	if err != nil {
		tk.Error = errors.WithStack(ErrRedisInvalid)
		return
	}
	// add group prefix to spilt difference group
	ops.redisPeriodKey = ops.group + "." + ops.redisPeriodKey
	rd := rs.MakeRedisClient().(redis.UniversalClient)
	client := asynq.NewClient(rs)
	inspector := asynq.NewInspector(rs)
	// initialize redis lock
	nxLock := lock.NewNxLock(
		lock.WithNxLockRedis(rd),
		lock.WithNxLockExpiration(10),
		lock.WithNxLockKey(ops.redisPeriodKey+".lock"),
	)
	// initialize server
	srv := asynq.NewServer(
		rs,
		asynq.Config{
			Concurrency: 10,
			Queues: map[string]int{
				ops.group: 10,
			},
		},
	)
	go func() {
		var h periodTaskHandler
		h.tk = *tk
		if e := srv.Run(h); e != nil {
			log.WithError(err).Error("run task handler failed")
		}
	}()
	tk.ops = *ops
	tk.redis = rd
	tk.redisOpt = rs
	tk.lock = nxLock
	tk.client = client
	tk.inspector = inspector
	// initialize scanner
	go func() {
		for {
			time.Sleep(time.Second)
			tk.scan()
		}
	}()
	if tk.ops.clearArchived > 0 {
		// initialize clear archived
		go func() {
			for {
				time.Sleep(time.Duration(tk.ops.clearArchived) * time.Second)
				tk.clearArchived()
			}
		}()
	}
	return
}

func (wk Worker) Once(options ...func(*RunOptions)) (err error) {
	ops := getRunOptionsOrSetDefault(nil)
	for _, f := range options {
		f(ops)
	}
	if ops.uid == "" {
		err = errors.WithStack(ErrUuidNil)
		return
	}
	t := asynq.NewTask(ops.category+".once", []byte(ops.payload), asynq.TaskID(ops.uid))
	taskOpts := []asynq.Option{
		asynq.Queue(wk.ops.group),
		asynq.MaxRetry(wk.ops.maxRetry),
		asynq.Timeout(time.Duration(ops.timeout) * time.Second),
	}
	if ops.maxRetry > 0 {
		taskOpts = append(taskOpts, asynq.MaxRetry(ops.maxRetry))
	}
	if ops.retention > 0 {
		taskOpts = append(taskOpts, asynq.Retention(time.Duration(ops.retention)*time.Second))
	} else {
		taskOpts = append(taskOpts, asynq.Retention(time.Duration(wk.ops.retention)*time.Second))
	}
	if ops.in != nil {
		taskOpts = append(taskOpts, asynq.ProcessIn(*ops.in))
	} else if ops.at != nil {
		taskOpts = append(taskOpts, asynq.ProcessAt(*ops.at))
	} else if ops.now {
		taskOpts = append(taskOpts, asynq.ProcessIn(time.Second))
	}
	_, err = wk.client.Enqueue(t, taskOpts...)
	return
}

func (wk Worker) Cron(options ...func(*RunOptions)) (err error) {
	ops := getRunOptionsOrSetDefault(nil)
	for _, f := range options {
		f(ops)
	}
	if ops.uid == "" {
		err = errors.WithStack(ErrUuidNil)
		return
	}
	var next int64
	next, err = getNext(ops.expr, 0)
	if err != nil {
		err = errors.WithStack(ErrExprInvalid)
		return
	}
	t := periodTask{
		Expr:     ops.expr,
		Name:     ops.category + ".cron",
		Uid:      ops.uid,
		Payload:  ops.payload,
		Next:     next,
		MaxRetry: ops.maxRetry,
		Timeout:  ops.timeout,
	}
	_, err = wk.redis.HSet(context.Background(), wk.ops.redisPeriodKey, ops.uid, t.String()).Result()
	if err != nil {
		err = errors.WithStack(ErrSaveCron)
		return
	}
	return
}

func (wk Worker) Remove(uid string) (err error) {
	var ok bool
	for {
		ok = wk.lock.Lock()
		if ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer wk.lock.Unlock()
	wk.redis.HDel(context.Background(), wk.ops.redisPeriodKey, uid)

	err = wk.inspector.DeleteTask(wk.ops.group, uid)
	return
}

func (wk Worker) processed(uid string) {
	var ok bool
	for {
		ok = wk.lock.Lock()
		if ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer wk.lock.Unlock()
	ctx := context.Background()
	t, e := wk.redis.HGet(ctx, wk.ops.redisPeriodKey, uid).Result()
	if e == nil || e != redis.Nil {
		var item periodTask
		item.FromString(t)
		item.Processed++
		wk.redis.HSet(ctx, wk.ops.redisPeriodKey, uid, item.String())
	}
	return
}

func (wk Worker) scan() {
	ctx := context.Background()
	ok := wk.lock.Lock()
	if !ok {
		return
	}
	defer wk.lock.Unlock()
	m, _ := wk.redis.HGetAll(ctx, wk.ops.redisPeriodKey).Result()
	p := wk.redis.Pipeline()
	ops := wk.ops
	for _, v := range m {
		var item periodTask
		item.FromString(v)
		next, _ := getNext(item.Expr, item.Next)
		t := asynq.NewTask(item.Name, []byte(item.Payload), asynq.TaskID(item.Uid))
		taskOpts := []asynq.Option{
			asynq.Queue(ops.group),
			asynq.MaxRetry(ops.maxRetry),
			asynq.Timeout(time.Duration(item.Timeout) * time.Second),
		}
		if item.MaxRetry > 0 {
			taskOpts = append(taskOpts, asynq.MaxRetry(item.MaxRetry))
		}
		diff := next - item.Next
		if diff > 10 {
			retention := diff / 3
			if diff > 600 {
				// max retention 10min
				retention = 600
			}
			// set retention avoid repeat in short time
			taskOpts = append(taskOpts, asynq.Retention(time.Duration(retention)*time.Second))
		}
		taskOpts = append(taskOpts, asynq.ProcessAt(time.Unix(item.Next, 0)))
		_, err := wk.client.Enqueue(t, taskOpts...)
		// enqueue success, update next
		if err == nil {
			item.Next = next
			p.HSet(ctx, wk.ops.redisPeriodKey, item.Uid, item.String())
		}
	}
	// batch save to cache
	p.Exec(ctx)
	return
}

func (wk Worker) clearArchived() {
	list, err := wk.inspector.ListArchivedTasks(wk.ops.group, asynq.Page(1), asynq.PageSize(100))
	if err != nil {
		return
	}
	ctx := context.Background()
	for _, item := range list {
		last := carbon.Time2Carbon(item.LastFailedAt)
		if !last.IsZero() && item.Retried < item.MaxRetry {
			continue
		}
		uid := item.ID
		var flag bool
		if strings.HasSuffix(item.Type, ".cron") {
			// cron task
			t, e := wk.redis.HGet(ctx, wk.ops.redisPeriodKey, uid).Result()
			if e == nil || e != redis.Nil {
				var task periodTask
				task.FromString(t)
				next, _ := getNext(task.Expr, task.Next)
				diff := next - task.Next
				if diff <= 60 {
					if carbon.Now().Gt(last.AddMinutes(5)) {
						flag = true
					}
				} else if diff <= 600 {
					if carbon.Now().Gt(last.AddMinutes(30)) {
						flag = true
					}
				} else if diff <= 3600 {
					if carbon.Now().Gt(last.AddHours(2)) {
						flag = true
					}
				} else {
					if carbon.Now().Gt(last.AddHours(5)) {
						flag = true
					}
				}
			}
		} else {
			// once task, has failed for more than 5 minutes
			if carbon.Now().Gt(last.AddMinutes(5)) {
				flag = true
			}
		}
		if flag {
			wk.inspector.DeleteTask(wk.ops.group, uid)
		}
	}
}

func getNext(expr string, timestamp int64) (next int64, err error) {
	var e *cronexpr.Expression
	e, err = cronexpr.Parse(expr)
	if err != nil {
		return
	}
	t := carbon.Now().Carbon2Time()
	if timestamp > 0 {
		t = carbon.CreateFromTimestamp(timestamp).Carbon2Time()
	}
	next = e.Next(t).Unix()
	return
}
