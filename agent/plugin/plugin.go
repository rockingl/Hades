package plugin

import (
	"agent/agent"
	"agent/proto"
	"agent/transport"
	"agent/utils"
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type Plugin struct {
	config proto.Config
	mu     sync.Mutex // according to uber_go_guide, pointer of mutex is not needed
	cmd    *exec.Cmd
	// rx/tx, all from the agent perspective
	rx      io.ReadCloser
	rxBytes uint64
	rxCnt   uint64
	tx      io.WriteCloser
	txBytes uint64
	txCnt   uint64

	updateTime time.Time
	reader     *bufio.Reader
	taskCh     chan proto.Task
	done       chan struct{} // same with the context done
	wg         *sync.WaitGroup
	workdir    string
	// SugaredLogger/Logger
	logger *zap.SugaredLogger
}

func NewPlugin(ctx context.Context, config proto.Config) (p *Plugin, err error) {
	var (
		rx_r, rx_w, tx_r, tx_w *os.File
		errFile                *os.File
	)
	p = &Plugin{
		config:     config,
		updateTime: time.Now(),
		done:       make(chan struct{}),
		taskCh:     make(chan proto.Task),
		wg:         &sync.WaitGroup{},
		logger:     zap.S().With("plugin", config.Name, "pver", config.Version, "psign", config.Signature),
	}
	p.workdir = path.Join(agent.Instance.Workdir, "plugin", p.Name())
	// pipe init
	// In Elkeid, a note: 'for compatibility' is here. Since some systems only allow
	// half-duplex pipe.
	rx_r, rx_w, err = os.Pipe()
	if err != nil {
		p.logger.Error("rx pipe init")
		return
	}
	p.rx = rx_r
	defer rx_w.Close()
	tx_r, tx_w, err = os.Pipe()
	if err != nil {
		p.logger.Error("tx pipe init")
		return
	}
	p.tx = tx_w
	defer tx_r.Close()
	// reader init
	p.reader = bufio.NewReaderSize(rx_r, 1024*128)
	// purge the files
	os.Remove(path.Join(p.workdir, p.Name()+".stderr"))
	os.Remove(path.Join(p.workdir, p.Name()+".stdout"))
	// cmdline
	execPath := path.Join(p.workdir, p.Name())
	err = utils.CheckSignature(execPath, config.Signature)
	if err != nil {
		p.logger.Warn("check signature failed")
		p.logger.Info("start download")
		err = utils.Download(ctx, execPath, config.Sha256, config.DownloadUrls, config.Type)
		if err != nil {
			p.logger.Error("download failed:", err)
			return
		}
		p.logger.Info("download success")
	}
	cmd := exec.Command(execPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = append(cmd.ExtraFiles, tx_r, rx_w)
	cmd.Dir = p.workdir
	if errFile, err = os.OpenFile(execPath+".stderr", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o0600); err != nil {
		p.logger.Error("open stderr:", errFile)
		return
	}
	defer errFile.Close()
	cmd.Stderr = errFile
	// details. if it is needed
	if config.Detail != "" {
		cmd.Env = append(cmd.Env, "DETAIL="+config.Detail)
	}
	p.logger.Info("cmd start")
	err = cmd.Start()
	if err != nil {
		p.logger.Error("cmd start:", err)
	}
	p.cmd = cmd
	return
}

func (p *Plugin) Wait() (err error) {
	defer p.wg.Done()
	err = p.cmd.Wait()
	p.rx.Close()
	p.tx.Close()
	close(p.done)
	return
}

func (p *Plugin) GetState() (RxSpeed, TxSpeed, RxTPS, TxTPS float64) {
	now := time.Now()
	instant := now.Sub(p.updateTime).Seconds()
	if instant != 0 {
		RxSpeed = float64(atomic.SwapUint64(&p.rxBytes, 0)) / float64(instant)
		TxSpeed = float64(atomic.SwapUint64(&p.txBytes, 0)) / float64(instant)
		RxTPS = float64(atomic.SwapUint64(&p.rxCnt, 0)) / float64(instant)
		TxTPS = float64(atomic.SwapUint64(&p.txCnt, 0)) / float64(instant)
	}
	p.updateTime = now
	return
}

func (p *Plugin) Name() string { return p.config.Name }

func (p *Plugin) Version() string { return p.config.Version }

func (p *Plugin) Pid() int { return p.cmd.Process.Pid }

// get the state by fork status
func (p *Plugin) IsExited() bool { return p.cmd.ProcessState != nil }

func (p *Plugin) Shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.IsExited() {
		return
	}
	p.logger.Info("shutdown called")
	p.tx.Close()
	p.rx.Close()
	select {
	case <-time.After(time.Second * 30):
		p.logger.Warn("close by killing start")
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
		p.logger.Info("close by killing done")
	case <-p.done:
		p.logger.Info("close by done channel")
	}
}

func (p *Plugin) Receive() {
	var (
		rec *proto.Record
		err error
	)
	defer p.wg.Done()
	for {
		if rec, err = p.receiveDataWithSize(); err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				// problem of multi
				p.logger.Warn("buffer full, skip")
				continue
				// any error about close or EOF, it's done
			} else if !(errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)) {
				p.logger.Error("receive err:", err)
				continue
			} else {
				p.logger.Error("exit the receive task:", err)
				break
			}
		}
		// fmt.Println(rec)
		transport.DTransfer.Transmission(rec, false)
	}
}

func (p *Plugin) Task() {
	var err error
	defer p.wg.Done()
	for {
		select {
		case <-p.done:
			return
		case task := <-p.taskCh:
			s := task.Size()
			var dst = make([]byte, 4+s)
			_, err = task.MarshalToSizedBuffer(dst[4:])
			if err != nil {
				p.logger.Errorf("task: %+v, err: %v", task, err)
				continue
			}
			binary.LittleEndian.PutUint32(dst[:4], uint32(s))
			var n int
			n, err = p.tx.Write(dst)
			if err != nil {
				if !errors.Is(err, os.ErrClosed) {
					p.logger.Error("when sending task, an error occurred: ", err)
				}
				return
			}
			atomic.AddUint64(&p.txCnt, 1)
			atomic.AddUint64(&p.txBytes, uint64(n))
		}
	}
}

// In Elkeid, receiveData get the data by decoding the data by self-code
// which performs better. For now, we work in an native way.
func (p *Plugin) receiveDataWithSize() (rec *proto.Record, err error) {
	var l uint32
	err = binary.Read(p.reader, binary.LittleEndian, &l)
	if err != nil {
		return
	}
	// TODO: sync.Pool
	rec = &proto.Record{}
	// TODO: sync.Pool, discard by cap
	// issues: https://github.com/golang/go/issues/23199
	// solutions: https://github.com/golang/go/blob/7e394a2/src/net/http/h2_bundle.go#L998-L1043
	message := make([]byte, int(l))
	if _, err = io.ReadFull(p.reader, message); err != nil {
		return
	}
	if err = rec.Unmarshal(message); err != nil {
		return
	}
	// Incr for plugin status
	atomic.AddUint64(&p.txCnt, 1)
	atomic.AddUint64(&p.txBytes, uint64(l))
	return
}

func (p *Plugin) SendTask(task proto.Task) (err error) {
	select {
	case p.taskCh <- task:
	default:
		err = errors.New("plugin is processing task or context has been canceled")
	}
	return
}

func (p *Plugin) GetWorkingDirectory() string {
	return p.cmd.Dir
}

func init() {
	go func() {
		for {
			select {
			case task := <-transport.PluginTaskChan:
				// In future, shutdown, update, restart will be in here
				if plg, ok := DefaultManager.Get(task.GetObjectName()); ok {
					if err := plg.SendTask(*task); err != nil {
						zap.S().Error("send task to plugin: ", err)
					}
				} else {
					zap.S().Error("can't find plugin: ", task.GetObjectName())
				}
			case cfgs := <-transport.PluginConfigChan:
				if err := DefaultManager.Sync(cfgs); err != nil {
					zap.S().Error("config sync failed: ", err)
				}
			}
		}
	}()
}
