package main

//go:generate rice embed-go

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sync"
	"time"

	rice "github.com/GeertJohan/go.rice"
	"github.com/gorilla/websocket"
	"github.com/masterzen/winrm"
	"github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	// message type transmitted to the view
	respMachine = "machine"
)

var (
	err      error
	exitCode int
	// Conf is the json configuration
	Conf Config
	// websocket
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	wsconn *websocket.Conn
	wserr  error
)

// json configuration
type Config struct {
	Main ConfigMain   `json:"main"`
	Dpts []*ConfigDpt `json:"dpts"`
}
type ConfigMain struct {
	AppScheme     string // not in conf file, set up in init
	AppBasePath   string // not in conf file, set up in init
	AppLocation   string // not in conf file, set up in init
	NoWindowsTest bool   // not in conf file, set up in init
	NoLinuxTest   bool   // not in conf file, set up in init
	NoPingTest    bool   // not in conf file, set up in init
}
type ConfigDpt struct {
	Title      string        `json:"title"`
	Connection ConfigConn    `json:"connection"`
	Machines   []*ConfigMach `json:"machines"`
}
type ConfigConn struct {
	WinrmPort     int                   `json:"winrmPort"`
	WinrmHTTPS    bool                  `json:"winrmHTTPS"`
	WinrmInsecure bool                  `json:"winrmInsecure"`
	WinrmTimeout  time.Duration         `json:"winrmTimeout"`
	WinrmUser     string                `json:"winrmUser"`
	WinrmPass     string                `json:"winrmPass"`
	SshTimeout    time.Duration         `json:"sshTimeout"`
	SshPort       int                   `json:"sshPort"`
	SshUser       string                `json:"sshUser"`
	SshPem        string                `json:"sshPem"`
	SshConfig     *ssh.ClientConfig     // not in conf file, set up in init
	LinuxCommand  []*LinuxCommandConfig `json:"linuxCommands"`
}
type LinuxCommandConfig struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}
type ConfigMach struct {
	Name  string   `json:"name"`
	Nb    int      `json:"nb"`
	Range []string // not in conf file, set up in init
}

// Machine is a structure representing a machine
type Machine struct {
	Name string `json:"name"`
	OS   string `json:"os"`
	Room string `json:"room"`
}

// Room is a structure representing a room
type Room struct {
	Name   string `json:"name"`
	NbMach int    `json:"nbmach"`
}

// WSResp is a structure sent to the view by JSON by web socket
type WSResp struct {
	Type     string           `json:"type"`
	Mach     Machine          `json:"mach"`
	CommandR []*CommandResult `json:"commandr"`
}
type CommandResult struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

// WSReaderWriter is a mutex websocket writer
type WSReaderWriter struct {
	uuid uuid.UUID
	ws   *websocket.Conn
	muxr sync.Mutex
	muxw sync.Mutex
}

// Send writes the json to the websocket
func (wss *WSReaderWriter) Send(json []byte) error {
	wss.muxw.Lock()
	defer wss.muxw.Unlock()
	err = wss.ws.WriteMessage(websocket.TextMessage, json)
	if err != nil {
		log.Error(err)
		return err
	}
	return nil
}

// Read reads the json from the websocket
func (wss *WSReaderWriter) Read() (*Room, error) {
	wss.muxr.Lock()
	defer wss.muxr.Unlock()
	r := Room{}
	err := wss.ws.ReadJSON(&r)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	return &r, nil
}

// WSReaderWriter constructor
func NewWSsender(w http.ResponseWriter, r *http.Request) *WSReaderWriter {
	// opening the websocket
	wss, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		panic("error opening socket:" + err.Error())
	}
	u := uuid.Must(uuid.NewV4())
	return &WSReaderWriter{uuid: u, ws: wss, muxr: sync.Mutex{}}
}

func executeCommand(command, hostname string, conn *ConfigConn) (string, error) {
	var (
		sshClient  *ssh.Client
		sshSession *ssh.Session
		stdoutBuf  bytes.Buffer
		e          error
	)
	log.Debug(fmt.Sprintf("before dial for %s at %s\n", hostname, time.Now()))
	if sshClient, e = ssh.Dial("tcp", fmt.Sprintf("%s:%d", hostname, conn.SshPort), conn.SshConfig); e != nil {
		return "", e
	}
	log.Debug(fmt.Sprintf("after dial for %s at %s\n", hostname, time.Now()))
	if sshSession, e = sshClient.NewSession(); e != nil {
		return "", e
	}

	defer func() {
		sshSession.Close()
		sshClient.Close()
	}()
	sshSession.Stdout = &stdoutBuf
	if e = sshSession.Run(command); e != nil {
		return "", e
	}
	log.Debug(fmt.Sprintf("after command for %s at %s\n", hostname, time.Now()))
	return stdoutBuf.String(), nil
}

// SSH key loader
func PublicKeyFile(file string) ssh.AuthMethod {
	buffer, err := ioutil.ReadFile(file)
	if err != nil {
		log.Debug(fmt.Sprintf("error reading ssh private key file: %s", err.Error))
		panic(err)
	}

	key, err := ssh.ParsePrivateKey(buffer)
	if err != nil {
		log.Debug(fmt.Sprintf("error parsing ssh private key file: %s", err.Error))
		panic(err)
	}
	return ssh.PublicKeys(key)
}

// SocketHandler handles the websocket communications
func SocketHandler(w http.ResponseWriter, r *http.Request) {
	// opening the websocket
	wss := NewWSsender(w, r)

	defer func() {
		log.Debug("closing websocket:" + wss.uuid.String())
		wss.ws.Close()
	}()

	for {
		log.Debug("entered SocketHandler loop with WSsender:" + wss.uuid.String())
		// receive the room to load/reload with name like infa12
		room, err := wss.Read()
		if err != nil {
			log.Debug("closing websocket on read error:" + wss.uuid.String())
			wss.ws.Close()
			break
		}

		// searching for the room nb of machines and connection info
		nbmach := 0
		var conn ConfigConn

		for _, d := range Conf.Dpts {
			for _, m := range d.Machines {
				if m.Name == room.Name {
					nbmach = m.Nb
					conn = d.Connection
					break
				}
			}
		}

		// looping throught the machines
		for i := 01; i <= nbmach; i++ {

			go func(machine int, room string, conn *ConfigConn, wss *WSReaderWriter) {

				// winrm and ssh connections
				var (
					winrmClient *winrm.Client
					out         string
					err         error
					Resp        WSResp
				)

				// building the cname
				cname := room + fmt.Sprintf("%02d", machine)
				log.Debug(fmt.Sprintf("machine %s\n", cname))

				if !Conf.Main.NoPingTest {
					// trying a ping
					log.Debug(fmt.Sprintf("  %s ping test\n", cname))
					//var cmdOut []byte
					if _, err = exec.Command("ping", "-c 1", "-W 1", cname).Output(); err != nil {
						log.Debug(fmt.Sprintf("  %s -> no ping: %s\n", cname, err.Error()))
						// returning the JSON
						Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "down", Room: room}}
						myJSON, jerr := json.Marshal(Resp)
						if jerr != nil {
							log.Debug(fmt.Sprintf("%s json marshal error: %s\n", cname, jerr.Error()))
							return
						}
						err := wss.Send(myJSON)
						if err != nil {
							log.Debug("closing websocket on write error:" + wss.uuid.String())
							wss.ws.Close()
							return
						}
						return
					}
					//fmt.Println(cmdOut)
				}

				if !Conf.Main.NoWindowsTest {
					// trying a windows connection
					log.Debug(fmt.Sprintf("  %s windows endpoint creation at %s\n", cname, time.Now()))
					endpoint := winrm.NewEndpoint(cname, conn.WinrmPort, conn.WinrmHTTPS, conn.WinrmInsecure, nil, nil, nil, conn.WinrmTimeout*time.Second)
					// https://en.wikipedia.org/wiki/ISO_8601#Durations
					// TODO: timeout does not work
					//param := winrm.NewParameters("PT"+conn.WinrmTimeout.String()+"S", "en-US", 153600)
					param := winrm.DefaultParameters
					if winrmClient, err = winrm.NewClientWithParameters(endpoint, conn.WinrmUser, conn.WinrmPass, param); err != nil {
						panic(err)
					}
					log.Debug(fmt.Sprintf("  %s windows endpoint done at %s\n", cname, time.Now()))

					// trying a simple dir command
					//if exitCode, err = winrmClient.Run("dir", os.Stdout, os.Stderr); err != nil {
					if exitCode, err = winrmClient.Run("ipconfig", ioutil.Discard, os.Stderr); err != nil {
						// machine under Linux or down
						log.Debug(fmt.Sprintf("  %s -> not windows (winrm command) at %s: %s\n", cname, time.Now(), err.Error()))
					} else if exitCode == 0 {
						// machine under windows
						// returning the JSON
						Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "windows", Room: room}}
						myJSON, jerr := json.Marshal(Resp)
						if jerr != nil {
							log.Debug(fmt.Sprintf("%s json marshal error: %s\n", cname, jerr.Error()))
							return
						}
						err := wss.Send(myJSON)
						if err != nil {
							log.Debug("closing websocket on write error:" + wss.uuid.String())
							wss.ws.Close()
							return
						}
						log.Debug(fmt.Sprintf("  %s -> windows at %s\n", cname, time.Now()))
						return
					}
				}

				if !Conf.Main.NoLinuxTest {
					// trying linux
					log.Debug(fmt.Sprintf("  %s linux test at %s\n", cname, time.Now()))
					if out, err = executeCommand("ls", cname, conn); err != nil {
						// can not run command - machine probably under Linux
						// returning the JSON
						Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "unknown", Room: room}}
						myJSON, jerr := json.Marshal(Resp)
						if jerr != nil {
							log.Debug(fmt.Sprintf("%s json marshal error: %s\n", cname, jerr.Error()))
							return
						}
						err := wss.Send(myJSON)
						if err != nil {
							log.Debug("closing websocket on write error:" + wss.uuid.String())
							wss.ws.Close()
							return
						}
						log.Debug(fmt.Sprintf("  %s -> not linux (ssh remote command error) at %s: \n", cname, time.Now()))
						return
					}
					// we are in linux, performing the linux commands
					log.Debug(fmt.Sprintf("  %s command test at %s\n", cname, time.Now()))
					Resp = WSResp{Type: respMachine, Mach: Machine{Name: cname, OS: "linux", Room: room}}
					for _, c := range conn.LinuxCommand {
						log.Debug(fmt.Sprintf("  %s command begin %s at %s\n", cname, c.Name, time.Now()))
						if out, err = executeCommand(c.Command, cname, conn); err != nil {
						} else {
							Resp.CommandR = append(Resp.CommandR, &CommandResult{Name: c.Name, Result: out})
						}
						log.Debug(fmt.Sprintf("  %s command end %s at %s\n", cname, c.Name, time.Now()))
					}
					// returning the JSON
					myJSON, jerr := json.Marshal(Resp)
					if jerr != nil {
						log.Debug(fmt.Sprintf("%s json marshal error: %s\n", cname, jerr.Error()))
						return
					}

					err := wss.Send(myJSON)
					if err != nil {
						log.Debug("closing websocket on write error:" + wss.uuid.String())
						wss.ws.Close()
						return
					}
					log.Debug(fmt.Sprintf("  %s -> linux at %s\n", cname, time.Now()))
				}
			}(i, room.Name, &conn, wss)
		}
	}
}

func (c *Config) MainHandler(w http.ResponseWriter, r *http.Request) {
	staticBox := rice.MustFindBox("static")

	// Building the HTML template.
	htmlTplString, err := staticBox.String("index.html")
	htmlTmp, err := template.New("index").Parse(htmlTplString)
	if err != nil {
		log.Fatal(err)
	}
	err = htmlTmp.Execute(w, c)
	if err != nil {
		log.Fatal(err)
	}
}

func init() {

	// loading json configuration file
	var (
		cf *os.File
	)
	if cf, err = os.Open("./configuration.json"); err != nil {
		panic(err)
	}
	decoder := json.NewDecoder(cf)
	if err = decoder.Decode(&Conf); err != nil {
		panic(err)
	}

	// initializing machine ranges
	// and departments SshConfig
	for _, d := range Conf.Dpts {

		d.Connection.SshConfig = &ssh.ClientConfig{
			User: d.Connection.SshUser,
			Auth: []ssh.AuthMethod{
				PublicKeyFile(d.Connection.SshPem),
			},
			Timeout: d.Connection.SshTimeout * time.Second,
			HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
				return nil
			},
		}

		for _, m := range d.Machines {
			for i := 1; i <= m.Nb; i++ {
				m.Range = append(m.Range, fmt.Sprintf("%02d", i))
			}
		}
	}
}

// request logger
func logRequest(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//log.Debug(fmt.Sprintf("%s %s %s\n", r.RemoteAddr, r.Method, r.URL))
		handler.ServeHTTP(w, r)
	})
}

func main() {

	// Getting the program parameters.
	listenPort := flag.String("port", "8081", "the port to listen")
	appScheme := flag.String("scheme", "http", "the application scheme, http or https")
	appBasePath := flag.String("base", "localhost:"+*listenPort, "the application base URL (such as localhost:8081)")
	appLocation := flag.String("location", "", "the application location with no leading or trailing / (such as gopcmap)")

	nowindowstest := flag.Bool("nowindows", false, "disable windows test")
	nolinuxtest := flag.Bool("nolinux", false, "disable linux test")
	nopingtest := flag.Bool("noping", false, "disable ping test")
	debug := flag.Bool("debug", false, "debug (verbose log), default is error")
	flag.Parse()
	Conf.Main.AppScheme = *appScheme
	Conf.Main.AppBasePath = *appBasePath
	Conf.Main.AppLocation = *appLocation
	Conf.Main.NoWindowsTest = *nowindowstest
	Conf.Main.NoLinuxTest = *nolinuxtest
	Conf.Main.NoPingTest = *nopingtest

	// setting the log level
	if *debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.ErrorLevel)
	}

	log.Info(fmt.Sprintf("AppBasePath: %s AppLocation: %s", Conf.Main.AppBasePath, Conf.Main.AppLocation))

	cssBox := rice.MustFindBox("static/css")
	locationCSS := "/css/"
	if *appLocation != "" {
		locationCSS = path.Join("/", *appLocation, "css")
		locationCSS += "/"
	}
	cssFileServer := http.StripPrefix(locationCSS, http.FileServer(cssBox.HTTPBox()))
	http.Handle(locationCSS, cssFileServer)

	// serving js
	jsBox := rice.MustFindBox("static/js")
	locationJS := "/js/"
	if *appLocation != "" {
		locationJS = path.Join("/", *appLocation, "js")
		locationJS += "/"
	}
	jsFileServer := http.StripPrefix(locationJS, http.FileServer(jsBox.HTTPBox()))
	http.Handle(locationJS, jsFileServer)

	// serving img
	imgBox := rice.MustFindBox("static/images")
	locationIMG := "/images/"
	if *appLocation != "" {
		locationIMG = path.Join("/", *appLocation, "images")
		locationIMG += "/"
	}
	imgFileServer := http.StripPrefix(locationIMG, http.FileServer(imgBox.HTTPBox()))
	http.Handle(locationIMG, imgFileServer)

	// serving websocket
	locationsocket := "/socket/"
	if *appLocation != "" {
		locationsocket = path.Join("/", *appLocation, "socket")
		locationsocket += "/"
	}
	http.HandleFunc(locationsocket, SocketHandler)

	http.HandleFunc("/", Conf.MainHandler)

	if err = http.ListenAndServe(":"+*listenPort, logRequest(http.DefaultServeMux)); err != nil {
		log.Fatal(err)
	}
}
