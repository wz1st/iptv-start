package until

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Child 是被监管的子进程（nginx / 管理系统 iptv / 授权引擎 engine）。
type Child struct {
	// Name 只用于日志，例如 "引擎" / "管理系统"。
	Name string
	// Bin 是可执行文件绝对路径。每次重启都重新解析，因此
	// 在线升级替换掉二进制后，重启即可生效。
	Bin  string
	Args []string
	// LogFile 非空时把子进程 stdout/stderr 重定向到该文件
	LogFile string
	// AutoRestart 为假时进程退出即结束监管（不再拉起）。
	AutoRestart bool
	MinDelay    time.Duration
	MaxDelay    time.Duration
	// HealthyAfter 见上方说明。
	HealthyAfter time.Duration

	mu        sync.Mutex
	cmd       *exec.Cmd
	logF      *os.File
	running   bool
	startedAt time.Time
	restarts  int
	lastErr   error
	stopped   bool

	kickCh chan struct{} // 请求"立刻重启"
	stopCh chan struct{} // 请求"停止监管"
	exitCh chan struct{} // 监管循环已退出
}

// Status 是子进程状态快照，直接作为 /status 的 JSON 字段输出。
type Status struct {
	Name          string `json:"name"`
	Bin           string `json:"bin"`
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	Restarts      int    `json:"restarts"`
	LastError     string `json:"lastError,omitempty"`
	UptimeSeconds int64  `json:"uptimeSeconds,omitempty"`
}

// NewChild 创建一个待监管的子进程（此时还没有拉起）。
func NewChild(name, bin string, args []string) *Child {
	return &Child{
		Name:         name,
		Bin:          bin,
		Args:         args,
		AutoRestart:  true,
		MinDelay:     time.Second,
		MaxDelay:     30 * time.Second,
		HealthyAfter: 60 * time.Second,
		kickCh:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		exitCh:       make(chan struct{}),
	}
}

// Start 拉起监管循环，并同步返回"第一次启动"的结果。
func (c *Child) Start() error {
	first := make(chan error, 1)
	go c.supervise(first)
	return <-first
}

// Restart 请求"立刻重启"。
func (c *Child) Restart() error {
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return errors.New("进程已停止")
	}
	select {
	case c.kickCh <- struct{}{}:
	default: // 已有一个待处理的重启请求，无需重复排队
	}
	return nil
}

// Stop 停止监管并结束子进程：先 SIGTERM 给收尾时间，超时再 SIGKILL。
// 可重复调用（第二次直接返回）。
func (c *Child) Stop(timeout time.Duration) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	pid := c.pidLocked()
	c.mu.Unlock()

	close(c.stopCh)

	if pid <= 0 {
		return
	}
	log.Printf("[%s] 正在停止 PID=%d ...", c.Name, pid)
	if c.killGroup(syscall.SIGTERM) {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if !c.Running() {
				log.Printf("[%s] 已停止", c.Name)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		log.Printf("[%s] 停止超时(%s)，强制结束", c.Name, timeout)
		c.killGroup(syscall.SIGKILL)
	}
}

// Running 表示子进程当前是否活着。
func (c *Child) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// PID 返回子进程 PID（未运行时为 0）。
func (c *Child) PID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pidLocked()
}

// Status 输出状态快照。
func (c *Child) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Name:     c.Name,
		Bin:      c.Bin,
		Running:  c.running,
		Restarts: c.restarts,
	}
	if c.running {
		st.PID = c.pidLocked()
		st.UptimeSeconds = int64(time.Since(c.startedAt).Seconds())
	}
	if c.lastErr != nil {
		st.LastError = errText(c.lastErr)
	}
	return st
}

// WaitExit 等待监管循环结束（用于退出流程里确认子进程已处理干净）。
func (c *Child) WaitExit(timeout time.Duration) {
	select {
	case <-c.exitCh:
	case <-time.After(timeout):
	}
}

// 内部实现

func (c *Child) supervise(first chan<- error) {
	defer close(c.exitCh)

	delay := c.MinDelay
	firstSent := false

	for {
		started := time.Now()
		err := c.launch()

		if !firstSent {
			firstSent = true
			first <- err
		}

		kicked := false
		if err != nil {
			// 连 exec 都没成功（文件不存在/无权限）：同样走退避重试，
			if !c.isStopped() {
				log.Printf("[%s] 启动失败: %v", c.Name, err)
			}
			c.markExited(err)
		} else {
			waitCh := make(chan error, 1)
			go func() { waitCh <- c.reap() }()

			select {
			case err = <-waitCh:
			case <-c.kickCh:
				kicked = true
				err = c.terminate(10*time.Second, waitCh)
			case <-c.stopCh:
				err = c.terminate(10*time.Second, waitCh)
				c.markExited(err)
				return
			}
			if kicked {
				// 主动重启不算异常，别把"被信号终止"记成故障
				c.markExited(nil)
			} else {
				c.markExited(err)
			}
		}

		if c.isStopped() || !c.AutoRestart {
			return
		}

		// 稳定跑够 HealthyAfter 才算"有效启动"，重置退避
		if time.Since(started) >= c.HealthyAfter {
			delay = c.MinDelay
		}
		n := c.bumpRestarts()

		if kicked {
			delay = 0
		} else if delay <= 0 {
			delay = c.MinDelay
		} else {
			delay = c.nextDelay(delay)
		}

		if kicked || delay == 0 {
			log.Printf("[%s] 按请求重启（累计第 %d 次）", c.Name, n)
		} else {
			log.Printf("[%s] 进程退出(%s)，%s 后重启（累计第 %d 次）", c.Name, errText(err), delay, n)
		}

		select {
		case <-time.After(delay):
		case <-c.kickCh: // 退避期间收到重启请求，跳过等待
		case <-c.stopCh:
			return
		}
	}
}

// launch 构造并启动子进程。
func (c *Child) launch() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return errors.New("进程已停止")
	}
	if c.running {
		return errors.New("进程已在运行")
	}

	cmd := exec.Command(c.Bin, c.Args...)
	cmd.Env = os.Environ()
	// 独立进程组：停止时能一次性带走 java / apktool / ffmpeg 等子孙进程
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var f *os.File
	if c.LogFile != "" {
		var err error
		// 与历史行为一致：每次启动截断，避免日志文件无限增长
		f, err = os.OpenFile(c.LogFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("打开日志文件%s失败: %w", c.LogFile, err)
		}
		cmd.Stdout, cmd.Stderr = f, f
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}

	if err := cmd.Start(); err != nil {
		if f != nil {
			f.Close()
		}
		return err
	}

	c.cmd = cmd
	c.logF = f
	c.running = true
	c.startedAt = time.Now()
	log.Printf("[%s] 已启动 %s (PID=%d)", c.Name, c.Bin, cmd.Process.Pid)
	return nil
}

// reap 等待当前子进程结束并回收资源。
func (c *Child) reap() error {
	c.mu.Lock()
	cmd, f := c.cmd, c.logF
	c.mu.Unlock()
	if cmd == nil {
		return nil
	}

	err := cmd.Wait()
	if f != nil {
		f.Close()
	}

	c.mu.Lock()
	c.running = false
	c.cmd = nil
	c.logF = nil
	c.mu.Unlock()
	return err
}

// terminate 向进程组发 SIGTERM，超时后 SIGKILL，返回进程退出错误。
func (c *Child) terminate(timeout time.Duration, waitCh <-chan error) error {
	if !c.killGroup(syscall.SIGTERM) {
		// 进程已经自己退出了，直接回收
		return <-waitCh
	}
	select {
	case err := <-waitCh:
		return err
	case <-time.After(timeout):
		log.Printf("[%s] 停止超时(%s)，强制结束", c.Name, timeout)
		c.killGroup(syscall.SIGKILL)
		return <-waitCh
	}
}

// killGroup 向子进程所在的进程组发信号，返回是否投递成功。
func (c *Child) killGroup(sig syscall.Signal) bool {
	pid := c.PID()
	if pid <= 0 {
		return false
	}
	// 负 PID = 整个进程组。进程已退出时返回 ESRCH，不影响后续 Wait。
	return syscall.Kill(-pid, sig) == nil
}

func (c *Child) pidLocked() int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *Child) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

func (c *Child) markExited(err error) {
	c.mu.Lock()
	c.running = false
	c.lastErr = err
	c.mu.Unlock()
}

func (c *Child) bumpRestarts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restarts++
	return c.restarts
}

func (c *Child) nextDelay(cur time.Duration) time.Duration {
	next := cur * 2
	if next > c.MaxDelay {
		next = c.MaxDelay
	}
	return next
}

// errText 把 exec 的退出错误整理成可读文本（nil 表示正常退出）。
func errText(err error) string {
	if err == nil {
		return "正常退出"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return "被信号 " + ws.Signal().String() + " 终止"
			}
			return fmt.Sprintf("退出码 %d", ws.ExitStatus())
		}
	}
	return err.Error()
}
