package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Centny/gwf/log"

	"github.com/Centny/gwf/util"
	"github.com/sutils/bsck"
	"github.com/sutils/dialer"
)

type Web struct {
	Suffix string `json:"suffix"`
	Listen string `json:"listen"`
	Auth   string `json:"auth"`
}

type Config struct {
	Name      string                `json:"name"`
	Cert      string                `json:"cert"`
	Key       string                `json:"key"`
	Listen    string                `json:"listen"`
	ACL       map[string]string     `json:"acl"`
	Socks5    string                `json:"socks5"`
	Web       Web                   `json:"web"`
	ShowLog   int                   `json:"showlog"`
	LogFlags  int                   `json:"logflags"`
	Forwards  map[string]string     `json:"forwards"`
	Channels  []*bsck.ChannelOption `json:"channels"`
	Dialer    util.Map              `json:"dialer"`
	Proxy     util.Map              `json:"proxy"`
	Reconnect int64                 `json:"reconnect"`
	RDPDir    string                `json:"rdp_dir"`
	VNCDir    string                `json:"vnc_dir"`
}

const Version = "1.4.0"

const RDPTmpl = `
screen mode id:i:2
use multimon:i:1
session bpp:i:24
full address:s:%v
audiomode:i:0
username:s:%v
disable wallpaper:i:0
disable full window drag:i:0
disable menu anims:i:0
disable themes:i:0
alternate shell:s:
shell working directory:s:
authentication level:i:2
connect to console:i:0
gatewayusagemethod:i:0
disable cursor setting:i:0
allow font smoothing:i:1
allow desktop composition:i:1
redirectprinters:i:0
prompt for credentials on client:i:0
bookmarktype:i:3
use redirection server name:i:0
`

const VNCTmpl = `
FriendlyName=%v
FullScreen=1
Host=%v
Password=%v
RelativePtr=0
Scaling=100%%
`

func readConfig(path string) (config *Config, last int64, err error) {
	var data []byte
	var dataFile *os.File
	var dataStat os.FileInfo
	dataFile, err = os.Open(path)
	if err == nil {
		dataStat, err = dataFile.Stat()
		if err == nil {
			last = dataStat.ModTime().Local().UnixNano() / 1e6
			data, err = ioutil.ReadAll(dataFile)
		}
		dataFile.Close()
	}
	if err == nil {
		user, _ := user.Current()
		config = &Config{
			RDPDir: filepath.Join(user.HomeDir, "Desktop"),
			VNCDir: filepath.Join(user.HomeDir, "Desktop"),
		}
		err = json.Unmarshal(data, config)
	}
	return
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-h" {
		fmt.Fprintf(os.Stderr, "Bond Socket Router Version %v\n", Version)
		fmt.Fprintf(os.Stderr, "Usage:  %v configure\n", "bsrouter")
		fmt.Fprintf(os.Stderr, "        %v /etc/bsrouter.json'\n", "bsrouter")
		fmt.Fprintf(os.Stderr, "bsrouter options:\n")
		fmt.Fprintf(os.Stderr, "        name\n")
		fmt.Fprintf(os.Stderr, "             the router name\n")
		fmt.Fprintf(os.Stderr, "        listen\n")
		fmt.Fprintf(os.Stderr, "             the master listen address\n")
		fmt.Fprintf(os.Stderr, "        forwards\n")
		fmt.Fprintf(os.Stderr, "             the forward uri by 'listen address':'uri'\n")
		fmt.Fprintf(os.Stderr, "        showlog\n")
		fmt.Fprintf(os.Stderr, "             the log level\n")
		fmt.Fprintf(os.Stderr, "        channels\n")
		fmt.Fprintf(os.Stderr, "             the channel configure\n")
		fmt.Fprintf(os.Stderr, "        channels.token\n")
		fmt.Fprintf(os.Stderr, "             the auth token to master\n")
		fmt.Fprintf(os.Stderr, "        channels.local\n")
		fmt.Fprintf(os.Stderr, "             the binded local address connect to master\n")
		fmt.Fprintf(os.Stderr, "        channels.remote\n")
		fmt.Fprintf(os.Stderr, "             the master address\n")
		os.Exit(1)
	}
	var config *Config
	var configLast int64
	var configPath string
	var err error
	if len(os.Args) > 1 {
		configPath = os.Args[1]
		config, configLast, err = readConfig(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read config from %v fail with %v\n", os.Args[1], err)
			os.Exit(1)
		}
	} else {
		u, _ := user.Current()
		for _, path := range []string{"./.bsrouter.json", "./bsrouter.json", u.HomeDir + "/.bsrouter/bsrouter.json", u.HomeDir + "/.bsrouter.json", "/etc/bsrouter/bsrouter.json", "/etc/bsrouer.json"} {
			config, configLast, err = readConfig(path)
			if err == nil || configLast > 0 {
				configPath = path
				fmt.Printf("bsrouter using config from %v\n", path)
				break
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "read config from .bsrouter.json or ~/.bsrouter.json or /etc/bsrouter/bsrouter.json or /etc/bsrouter.json fail with %v\n", err)
			os.Exit(1)
		}
	}
	if config.LogFlags > 0 {
		bsck.Log.SetFlags(config.LogFlags)
		log.SharedLogger().SetFlags(config.LogFlags)
	}
	bsck.ShowLog = config.ShowLog
	socks5 := bsck.NewSocksProxy()
	forward := bsck.NewForward()
	node := bsck.NewProxy(config.Name)
	node.Cert, node.Key = config.Cert, config.Key
	if config.Reconnect > 0 {
		node.ReconnectDelay = time.Duration(config.Reconnect) * time.Millisecond
	}
	if len(config.ACL) > 0 {
		node.ACL = config.ACL
	}
	dialer.NewDialer = func(t string) dialer.Dialer {
		if t == "router" {
			return NewRouterDialer(node.Router)
		}
		return dialer.DefaultDialerCreator(t)
	}
	dialerPool := dialer.NewPool()
	dialerPool.Bootstrap(config.Dialer)
	dialerProxy := dialer.NewBalancedDialer()
	err = dialerProxy.Bootstrap(config.Proxy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boot proxy dialer fail with %v\n", err)
		os.Exit(1)
	}
	var protocolMatcher = regexp.MustCompile("^[A-Za-z0-9]*://.*$")
	var dialerAll = func(uri string, raw io.ReadWriteCloser) (sid uint64, err error) {
		if strings.Contains(uri, "->") {
			sid, err = node.Dial(uri, raw)
			return
		}
		if protocolMatcher.MatchString(uri) {
			var conn dialer.Conn
			sid = node.UniqueSid()
			conn, err = dialerPool.Dial(sid, uri)
			if err == nil {
				err = conn.Pipe(raw)
			}
			return
		}
		parts := strings.SplitN(uri, "?", 2)
		router := forward.FindForward(parts[0])
		if len(router) < 1 {
			err = fmt.Errorf("forward not found by %v", uri)
			return
		}
		dialURI := router[1]
		if len(parts) > 1 {
			if strings.Contains(dialURI, "?") {
				dialURI += "&" + parts[1]
			} else {
				dialURI += "?" + parts[1]
			}
		}
		sid, err = node.Dial(dialURI, raw)
		return
	}
	socks5.Dialer = func(utype int, uri string, raw io.ReadWriteCloser) (sid uint64, err error) {
		switch utype {
		case bsck.SocksUriTypeBS:
			sid, err = dialerAll(uri, raw)
		default:
			var conn dialer.Conn
			sid = node.UniqueSid()
			conn, err = dialerProxy.Dial(sid, "tcp://"+uri)
			if err == nil {
				err = conn.Pipe(raw)
			}
		}
		return
	}
	forward.Dialer = dialerAll
	node.Handler = bsck.DialRawF(func(sid uint64, uri string) (conn bsck.Conn, err error) {
		raw, err := dialerPool.Dial(sid, uri)
		if err == nil {
			conn = bsck.NewRawConn(raw, sid, uri)
		}
		return
	})
	var confLck = sync.RWMutex{}
	var addFoward = func(loc, uri string) (err error) {
		target, err := url.Parse(loc)
		if err != nil {
			return
		}
		var rdp, vnc bool
		var listener net.Listener
		parts := strings.SplitAfterN(target.Host, ":", 2)
		switch target.Scheme {
		case "socks":
			if len(parts) > 1 {
				target.Host = ":" + parts[1]
			} else {
				target.Host = ":0"
			}
			target.Scheme = "socks"
			listener, err = node.StartForward(parts[0], target, uri)
		case "locsocks":
			if len(parts) > 1 {
				target.Host = "localhost:" + parts[1]
			} else {
				target.Host = "localhost:0"
			}
			target.Scheme = "socks"
			listener, err = node.StartForward(parts[0], target, uri)
		case "tcp":
			if len(parts) > 1 {
				target.Host = ":" + parts[1]
			} else {
				target.Host = ":0"
			}
			target.Scheme = "tcp"
			listener, err = node.StartForward(parts[0], target, uri)
		case "loctcp":
			if len(parts) > 1 {
				target.Host = "localhost:" + parts[1]
			} else {
				target.Host = "localhost:0"
			}
			target.Scheme = "tcp"
			listener, err = node.StartForward(parts[0], target, uri)
		case "rdp":
			rdp = true
			if len(parts) > 1 {
				target.Host = ":" + parts[1]
			} else {
				target.Host = ":0"
			}
			target.Scheme = "tcp"
			listener, err = node.StartForward(parts[0], target, uri)
		case "locrdp":
			rdp = true
			if len(parts) > 1 {
				target.Host = "localhost:" + parts[1]
			} else {
				target.Host = "localhost:0"
			}
			target.Scheme = "tcp"
			listener, err = node.StartForward(parts[0], target, uri)
		case "vnc":
			vnc = true
			if len(parts) > 1 {
				target.Host = ":" + parts[1]
			} else {
				target.Host = ":0"
			}
			target.Scheme = "tcp"
			listener, err = node.StartForward(parts[0], target, uri)
		case "locvnc":
			vnc = true
			if len(parts) > 1 {
				target.Host = "localhost:" + parts[1]
			} else {
				target.Host = "localhost:0"
			}
			target.Scheme = "tcp"
			listener, err = node.StartForward(parts[0], target, uri)
		case "unix":
			if len(parts) > 1 {
				target.Host = parts[1]
				listener, err = node.StartForward(parts[0], target, uri)
			} else {
				err = fmt.Errorf("the unix file is required")
			}
		default:
			err = forward.AddForward(loc, uri)
		}
		if err == nil && rdp && len(config.RDPDir) > 0 {
			confLck.Lock()
			fileData := fmt.Sprintf(RDPTmpl, listener.Addr(), target.User.Username())
			savepath := filepath.Join(config.RDPDir, parts[0]+".rdp")
			err := ioutil.WriteFile(savepath, []byte(fileData), os.ModePerm)
			confLck.Unlock()
			if err != nil {
				bsck.WarnLog("bsrouter save rdp info to %v faile with %v", savepath, err)
			} else {
				bsck.InfoLog("bsrouter save rdp info to %v success", savepath)
			}
		}
		if err == nil && vnc && len(config.VNCDir) > 0 {
			confLck.Lock()
			password, _ := target.User.Password()
			fileData := fmt.Sprintf(VNCTmpl, parts[0], listener.Addr(), password)
			savepath := filepath.Join(config.VNCDir, parts[0]+".vnc")
			err := ioutil.WriteFile(savepath, []byte(fileData), os.ModePerm)
			confLck.Unlock()
			if err != nil {
				bsck.WarnLog("bsrouter save vnc info to %v faile with %v", savepath, err)
			} else {
				bsck.InfoLog("bsrouter save vnc info to %v success", savepath)
			}
		}
		return
	}
	var removeFoward = func(loc string) (err error) {
		target, err := url.Parse(loc)
		if err != nil {
			return
		}
		var rdp, vnc bool
		parts := strings.SplitAfterN(target.Host, ":", 2)
		switch target.Scheme {
		case "socks":
			err = node.StopForward(parts[0])
		case "locsocks":
			err = node.StopForward(parts[0])
		case "tcp":
			err = node.StopForward(parts[0])
		case "loctcp":
			err = node.StopForward(parts[0])
		case "rdp":
			rdp = true
			err = node.StopForward(parts[0])
		case "locrdp":
			rdp = true
			err = node.StopForward(parts[0])
		case "vnc":
			vnc = true
			err = node.StopForward(parts[0])
		case "locvnc":
			vnc = true
			err = node.StopForward(parts[0])
		case "unix":
			if len(parts) > 1 {
				err = node.StopForward(parts[0])
			} else {
				err = fmt.Errorf("the unix file is required")
			}
		default:
			err = forward.RemoveForward(loc)
		}
		if rdp && len(config.RDPDir) > 0 {
			confLck.Lock()
			savepath := filepath.Join(config.RDPDir, parts[0]+".rdp")
			err := os.Remove(savepath)
			confLck.Unlock()
			if err != nil {
				bsck.WarnLog("bsrouter remove rdp file on %v faile with %v", savepath, err)
			} else {
				bsck.InfoLog("bsrouter remove rdp file on %v success", savepath)
			}
		}
		if vnc && len(config.VNCDir) > 0 {
			confLck.Lock()
			savepath := filepath.Join(config.VNCDir, parts[0]+".vnc")
			err := os.Remove(savepath)
			confLck.Unlock()
			if err != nil {
				bsck.WarnLog("bsrouter remove vnc file on %v faile with %v", savepath, err)
			} else {
				bsck.InfoLog("bsrouter remove vnc file on %v success", savepath)
			}
		}
		return
	}
	//
	//
	if len(config.Listen) > 0 {
		err := node.ListenMaster(config.Listen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "start master on %v fail with %v\n", config.Listen, err)
			os.Exit(1)
		}
	}
	if len(config.Channels) > 0 {
		go node.LoginChannel(true, config.Channels...)
	}
	for loc, uri := range config.Forwards {
		err = addFoward(loc, uri)
		if err != nil {
			bsck.ErrorLog("bsrouter add forward by %v->%v fail with %v", loc, uri, err)
			// os.Exit(1)
		}
	}
	node.StartHeartbeat()
	if len(config.Socks5) > 0 {
		err := socks5.Start(config.Socks5)
		if err != nil {
			fmt.Fprintf(os.Stderr, "start socks5 by %v fail with %v\n", config.Socks5, err)
			os.Exit(1)
		}
	}
	http.HandleFunc("/dav/", forward.ProcWebSubsH)
	http.HandleFunc("/ws/", forward.ProcWebSubsH)
	http.HandleFunc("/", forward.HostForwardF)
	forward.WebAuth = config.Web.Auth
	forward.WebSuffix = config.Web.Suffix
	server := &http.Server{Addr: config.Web.Listen}
	if len(config.Web.Listen) > 0 {
		go func() {
			bsck.Log.Printf("I bsrouter listen web on %v\n", config.Web.Listen)
			fmt.Println(server.ListenAndServe())
		}()
	}
	go func() { //auto reload forward
		for {
			time.Sleep(3 * time.Second)
			fileInfo, err := os.Stat(configPath)
			if err != nil {
				bsck.DebugLog("bsrouter reload configure fail with %v", err)
				break
			}
			newLast := fileInfo.ModTime().Local().UnixNano() / 1e6
			if newLast == configLast {
				continue
			}
			newConfig, newLast, err := readConfig(configPath)
			if err != nil {
				bsck.DebugLog("bsrouter reload configure fail with %v", err)
				break
			}
			bsck.DebugLog("bsrouter will reload modified configure %v by old(%v),new(%v)", configPath, configLast, newLast)
			//remove missing
			for loc := range config.Forwards {
				if _, ok := newConfig.Forwards[loc]; ok {
					continue
				}
				err = removeFoward(loc)
				if err != nil {
					bsck.ErrorLog("bsrouter remove forward by %v fail with %v", loc, err)
					// os.Exit(1)
				}
			}
			for loc, uri := range newConfig.Forwards {
				if config.Forwards[loc] == uri {
					continue
				}
				err = addFoward(loc, uri)
				if err != nil {
					bsck.ErrorLog("bsrouter add forward by %v->%v fail with %v", loc, uri, err)
					// os.Exit(1)
				}
			}
			config.Forwards = newConfig.Forwards
			configLast = newLast
		}
	}()
	wc := make(chan os.Signal, 1)
	signal.Notify(wc, os.Interrupt, os.Kill)
	<-wc
	fmt.Println("clear bsrouter...")
	node.Close()
	server.Close()
}

type RouterDialer struct {
	basic   *bsck.Router
	ID      string
	conf    util.Map
	matcher *regexp.Regexp
	router  string
	args    string
}

func NewRouterDialer(basic *bsck.Router) *RouterDialer {
	return &RouterDialer{
		basic:   basic,
		conf:    util.Map{},
		matcher: regexp.MustCompile("^.*$"),
	}
}

func (r *RouterDialer) Name() string {
	return r.ID
}

//initial dialer
func (r *RouterDialer) Bootstrap(options util.Map) (err error) {
	r.conf = options
	r.ID = options.StrVal("id")
	if len(r.ID) < 1 {
		return fmt.Errorf("the dialer id is required")
	}
	matcher := options.StrVal("matcher")
	if len(matcher) > 0 {
		r.matcher, err = regexp.Compile(matcher)
	}
	r.router = options.StrVal("router")
	if len(r.router) < 1 {
		err = fmt.Errorf("the dialer router is required")
	}
	r.args = options.StrVal("args")
	return
}

//
func (r *RouterDialer) Options() util.Map {
	return r.conf
}

//match uri
func (r *RouterDialer) Matched(uri string) bool {
	return r.matcher.MatchString(uri)
}

//dial raw connection
func (r *RouterDialer) Dial(sid uint64, uri string) (rw dialer.Conn, err error) {
	rw = &RouterConn{
		dialer: r,
		uri:    uri,
	}
	return
}

func (r *RouterDialer) Pipe(uri string, raw io.ReadWriteCloser) (sid uint64, err error) {
	sid, err = r.basic.Dial(strings.Replace(r.router, "${URI}", uri, -1), raw)
	return
}

type RouterConn struct {
	sid    uint64
	raw    io.ReadWriteCloser
	dialer *RouterDialer
	uri    string
}

func (r *RouterConn) Pipe(raw io.ReadWriteCloser) (err error) {
	r.raw = raw
	r.sid, err = r.dialer.Pipe(r.uri, raw)
	return
}

func (r *RouterConn) Read(p []byte) (n int, err error) {
	err = fmt.Errorf("RouterConn.Read is not impl")
	return
}

func (r *RouterConn) Write(p []byte) (n int, err error) {
	err = fmt.Errorf("RouterConn.Write is not impl")
	return
}

func (r *RouterConn) Close() (err error) {
	if r.raw != nil {
		err = r.raw.Close()
	}
	return
}
