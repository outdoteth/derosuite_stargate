// Copyright 2017-2018 DERO Project. All rights reserved.
// Use of this source code in any form is governed by RESEARCH license.
// license can be found in the LICENSE file.
// GPG: 0F39 E425 8C65 3947 702A  8234 08B2 0360 A03A 9DE8
//
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY
// EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL
// THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
// PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
// STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF
// THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package rpcserver

import "io"
import "os"
import "fmt"
import "net"
import "time"
import "context"
import "strings"
import "sync"
import "sync/atomic"
import "crypto/tls"

//import "context"
import "net/http"
import "net/http/pprof"

//import "github.com/intel-go/fastjson"
import "github.com/osamingo/jsonrpc"
import log "github.com/sirupsen/logrus"
import "github.com/prometheus/client_golang/prometheus"
import "github.com/prometheus/client_golang/prometheus/promhttp"

import "github.com/deroproject/derosuite/config"
import "github.com/deroproject/derosuite/globals"
import "github.com/deroproject/derosuite/blockchain"
import "github.com/deroproject/derosuite/structures"
import "github.com/deroproject/derosuite/metrics"

var DEBUG_MODE bool

/* this file implements the rpcserver api, so as wallet and block explorer tools can work without migration */

// all components requiring access to blockchain must use , this struct to communicate
// this structure must be update while mutex
type RPCServer struct {
	srv        *http.Server
	mux        *http.ServeMux
	Exit_Event chan bool // blockchain is shutting down and we must quit ASAP
	sync.RWMutex
}

var Exit_In_Progress bool
var chain *blockchain.Blockchain
var logger *log.Entry

func RPCServer_Start(params map[string]interface{}) (*RPCServer, error) {

	var err error
	var r RPCServer

	_ = err

	r.Exit_Event = make(chan bool)

	logger = globals.Logger.WithFields(log.Fields{"com": "RPC"}) // all components must use this logger
	chain = params["chain"].(*blockchain.Blockchain)

	/*
		// test whether chain is okay
		if chain.Get_Height() == 0 {
			return nil, fmt.Errorf("Chain DOES NOT have genesis block")
		}
	*/

	go r.Run()
	logger.Infof("RPC server started")
	atomic.AddUint32(&globals.Subsystem_Active, 1) // increment subsystem

	return &r, nil
}

// shutdown the rpc server component
func (r *RPCServer) RPCServer_Stop() {
	r.Lock()
	defer r.Unlock()
	Exit_In_Progress = true
	close(r.Exit_Event) // send signal to all connections to exit

	if r.srv != nil {
		r.srv.Shutdown(context.Background()) // shutdown the server
	}
	// TODO we  must wait for connections to kill themselves
	time.Sleep(1 * time.Second)
	logger.Infof("RPC Shutdown")
	atomic.AddUint32(&globals.Subsystem_Active, ^uint32(0)) // this decrement 1 fom subsystem
}

// setup handlers
func (r *RPCServer) Run() {

	mr := jsonrpc.NewMethodRepository()

	if err := mr.RegisterMethod("Main.Echo", EchoHandler{}, EchoParams{}, EchoResult{}); err != nil {
		log.Fatalln(err)
	}

	// install getblockcount handler
	if err := mr.RegisterMethod("getblockcount", GetBlockCount_Handler{}, structures.GetBlockCount_Params{}, structures.GetBlockCount_Result{}); err != nil {
		log.Fatalln(err)
	}

	// install on_getblockhash
	if err := mr.RegisterMethod("on_getblockhash", On_GetBlockHash_Handler{}, structures.On_GetBlockHash_Params{}, structures.On_GetBlockHash_Result{}); err != nil {
		log.Fatalln(err)
	}

	// install getblocktemplate handler
	if err := mr.RegisterMethod("getblocktemplate", GetBlockTemplate_Handler{}, structures.GetBlockTemplate_Params{}, structures.GetBlockTemplate_Result{}); err != nil {
		log.Fatalln(err)
	}

	// submitblock handler
	if err := mr.RegisterMethod("submitblock", SubmitBlock_Handler{}, structures.SubmitBlock_Params{}, structures.SubmitBlock_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("getlastblockheader", GetLastBlockHeader_Handler{}, structures.GetLastBlockHeader_Params{}, structures.GetLastBlockHeader_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("getblockheaderbyhash", GetBlockHeaderByHash_Handler{}, structures.GetBlockHeaderByHash_Params{}, structures.GetBlockHeaderByHash_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("getblockheaderbyheight", GetBlockHeaderByHeight_Handler{}, structures.GetBlockHeaderByHeight_Params{}, structures.GetBlockHeaderByHeight_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("getblockheaderbytopoheight", GetBlockHeaderByTopoHeight_Handler{}, structures.GetBlockHeaderByTopoHeight_Params{}, structures.GetBlockHeaderByHeight_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("getblock", GetBlock_Handler{}, structures.GetBlock_Params{}, structures.GetBlock_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("get_info", GetInfo_Handler{}, structures.GetInfo_Params{}, structures.GetInfo_Result{}); err != nil {
		log.Fatalln(err)
	}

	if err := mr.RegisterMethod("gettxpool", GetTxPool_Handler{}, structures.GetTxPool_Params{}, structures.GetTxPool_Result{}); err != nil {
		log.Fatalln(err)
	}
	
	if err := mr.RegisterMethod("is_key_image_spent", IsKeyImageSpent_Handler{}, structures.Is_Key_Image_Spent_Params{}, structures.Is_Key_Image_Spent_Result{}); err != nil {
		log.Fatalln(err)
	}

	// create a new mux
	r.mux = http.NewServeMux()

	default_address := "127.0.0.1:" + fmt.Sprintf("%d", config.Mainnet.RPC_Default_Port)
	if !globals.IsMainnet() {
		default_address = "127.0.0.1:" + fmt.Sprintf("%d", config.Testnet.RPC_Default_Port)
	}

	if _, ok := globals.Arguments["--rpc-bind"]; ok && globals.Arguments["--rpc-bind"] != nil {
		addr, err := net.ResolveTCPAddr("tcp", globals.Arguments["--rpc-bind"].(string))
		if err != nil {
			logger.Warnf("--rpc-bind address is invalid, err = %s", err)
		} else {
			if addr.Port == 0 {
				logger.Infof("RPC server is disabled, No ports will be opened for RPC")
				return
			} else {
				default_address = addr.String()
			}
		}
	}

	// if we pin the certificate, we can have the perfect score
	//  but for certain reasons, we are not pinning  them
	//w.Header().Add("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
	cfg := &tls.Config{
       /* MinVersion:               tls.VersionTLS12,
        CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
        PreferServerCipherSuites: true,
        CipherSuites: []uint16{
            tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
            tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
            tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
            tls.TLS_RSA_WITH_AES_256_CBC_SHA,
        },*/
    }
    
	logger.Infof("RPC  will listen on %s", default_address)
	r.Lock()
	r.srv = &http.Server{
            Addr: default_address,
            Handler: r.mux,
            ReadTimeout: 5 * time.Second,
            ReadHeaderTimeout: 5 * time.Second,
            WriteTimeout: 40 * time.Second,
            TLSConfig:    cfg,
            TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler), 0),
            
        }
	r.Unlock()

        
        // serve any static files from this directory
        r.mux.Handle("/static/", http.FileServer(http.Dir("./webroot")))
        
        
	//r.mux.HandleFunc("/", hello)
	r.mux.Handle("/json_rpc", mr)

	// handle nasty http requests
	r.mux.HandleFunc("/getheight", getheight)
	r.mux.HandleFunc("/getoutputs.bin", getoutputs) // stream any outputs to server, can make wallet work offline
	r.mux.HandleFunc("/gettransactions", gettransactions)
	r.mux.HandleFunc("/sendrawtransaction", SendRawTransaction_Handler)
	r.mux.HandleFunc("/is_key_image_spent", iskeyimagespent)

	if DEBUG_MODE {
		// r.mux.HandleFunc("/debug/pprof/", pprof.Index)

		// Register pprof handlers individually if required
		r.mux.HandleFunc("/debug/pprof/", pprof.Index)
		r.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		/*
		       // Register pprof handlers individually if required
		   r.mux.HandleFunc("/cdebug/pprof/", pprof.Index)
		   r.mux.HandleFunc("/cdebug/pprof/cmdline", pprof.Cmdline)
		   r.mux.HandleFunc("/cdebug/pprof/profile", pprof.Profile)
		   r.mux.HandleFunc("/cdebug/pprof/symbol", pprof.Symbol)
		   r.mux.HandleFunc("/cdebug/pprof/trace", pprof.Trace)
		*/

		// register metrics handler
		r.mux.HandleFunc("/metrics", prometheus.InstrumentHandler("dero", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})))

	}
	

	//r.mux.HandleFunc("/json_rpc/debug", mr.ServeDebug)
	
	//r.srv.SetKeepAlivesEnabled(true) 
	
	
  
            wrappedMux := NewInternalOrFileSystem(r.mux) 
            r.srv.Handler = wrappedMux;

        // the tls keys self certified can be generated using
        // openssl ecparam -genkey -name secp384r1 -out server.key
        // openssl req -new -x509 -sha256 -key server.key -out server.crt -days 3650

        if _, err := os.Stat("./keys/server.crt"); !os.IsNotExist(err){
            if _, err := os.Stat("./keys/server.key"); !os.IsNotExist(err){
                if err := r.srv.ListenAndServeTLS("keys/server.crt","keys/server.key"); err != http.ErrServerClosed {
                    logger.Warnf("ERR TLS listening to address err %s", err)
                }
            }
        }else{
            if err := r.srv.ListenAndServe(); err != http.ErrServerClosed {
		logger.Warnf("ERR listening to address err %s", err)
            }
        }

}


//this middleware see whether a request can be served from the filesystem
type InternalOrFileSystem struct {
    handler http.Handler
}

//ServeHTTP handles the request by passing it to the real
//handler 
func (l *InternalOrFileSystem) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    _ = start
    
    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Max-Age", "3600")
    w.Header().Set("Access-Control-Allow-Credentials", "true")
    w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, DELETE")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, X-Requested-With")
    
    // more protections
    w.Header().Set("X-Frame-Options", "DENY") // never load in an iframe
    w.Header().Set("X-XSS-Protection", "1; mode=block") //  do we need XSS protection, since everything is on client
    w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline' 'unsafe-eval' ; img-src 'self' data: ") // CSP disable access to all other domains

    
    // enable caching for static assets
    if strings.Contains(r.URL.Path,"/static") || r.URL.Path == "/" {
         w.Header().Set("Cache-Control", "public, max-age=7776000, must-revalidate")
    }
    
    
    // NOTE: redirect /  to wallet
    // NOTE: use hard-coded paths, other directory traversal attacks are possible
    if r.URL.Path == "/" ||  r.URL.Path == "/index.html" {
         //http.ServeFile(w, r, "./webroot/static/wallet.html") // this does not sent content-type
         
         file, err := os.Open("./webroot/static/wallet.html")
        if err != nil {
            w.WriteHeader(http.StatusInternalServerError)
            http.Error(w, fmt.Sprintf("Unable to open and read file : %v", err),404)
            return
        }
        defer file.Close()
        
        http.ServeContent(w, r, "index.html", time.Now(), file) // this will properly set the headers
        return
        }
            
    
    
    l.handler.ServeHTTP(w, r) // pass request to RPC handler
   // log.Printf("%s %s %v", r.Method, r.URL.Path, time.Since(start))
}

//NewLogger constructs a new middleware handler to hook some static file requests
func NewInternalOrFileSystem(handlerToWrap http.Handler) *InternalOrFileSystem {
    return &InternalOrFileSystem{handlerToWrap}
}


func hello(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "Hello world!")
}
