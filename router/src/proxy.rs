use async_trait::async_trait;
use bytes::Bytes;
use futures::Future;
use http_body_util::Full;
use hyper::body::Incoming;
use hyper::server::conn::http1;
use hyper::service::Service;
use hyper::{Request, Response, StatusCode};
use log::{info, warn};
use serde_json::json;
use smoltcp::iface::{Config, Interface};
use smoltcp::wire::{IpAddress, IpCidr};
use std::os::fd::{AsRawFd, RawFd};
use std::pin::Pin;
use std::time::Duration;
use std::{collections::HashMap, net::SocketAddr, sync::Arc};
use tokio::net::TcpSocket;
use tokio::sync::mpsc::Receiver;
use tokio::{
    net::{TcpListener, TcpStream},
    sync::Mutex,
};
use tokio_rustls::TlsConnector;

use crate::frame;
use crate::gateway::{CacheDevice, Gateway};
use crate::util::{json_must_obj, json_must_str, json_option_i64, json_option_obj, json_option_str, load_tls_config, wrap_err, JSON};
use crate::wrapper::{wrap_quinn_w, WrapUdpConn};
use crate::{
    router::{Handler, Router},
    util::new_message_err,
    wrapper::{wrap_split_tcp_w, wrap_split_tls_w},
};

#[derive(Clone)]
pub struct ServerState {
    pub name: Arc<String>,
    pub stopper: String,
    pub stopping: bool,
    pub addr: SocketAddr,
}

impl ServerState {
    pub fn new(name: Arc<String>, stopper: String, addr: SocketAddr) -> Self {
        Self { name, stopper, stopping: false, addr }
    }

    pub async fn stop(&mut self) {
        self.stopping = true;
        for _ in 0..3 {
            _ = TcpStream::connect(&self.stopper).await;
            tokio::time::sleep(tokio::time::Duration::from_millis(300)).await;
        }
    }

    pub fn to_string(&self) -> String {
        format!("Server(name:{},stopper:{})", self.name, self.stopper)
    }
}

#[async_trait]
pub trait Preparer {
    async fn prepare_fd(&self, fd: RawFd) -> tokio::io::Result<()>;
}

pub struct SkipPreparer {}

impl SkipPreparer {
    pub fn new() -> Self {
        Self {}
    }
}

#[async_trait]
impl Preparer for SkipPreparer {
    async fn prepare_fd(&self, _: RawFd) -> tokio::io::Result<()> {
        Ok(())
    }
}

#[derive(Clone)]
pub struct Proxy {
    pub name: Arc<String>,
    pub dir: Arc<String>,
    pub router: Arc<Router>,
    pub handler: Arc<dyn Handler + Send + Sync>,
    pub preparer: Arc<dyn Preparer + Send + Sync>,
    pub channels: HashMap<String, Arc<JSON>>,
    pub interval: u64,
    pub gateway: Gateway,
    listener: HashMap<String, Arc<Mutex<ServerState>>>,
    waiter: Arc<wg::AsyncWaitGroup>,
}

impl Proxy {
    pub fn new(name: Arc<String>, handler: Arc<dyn Handler + Send + Sync>) -> Self {
        let dir = Arc::new(String::from("."));
        let router = Arc::new(Router::new(name.clone(), handler.clone()));
        let waiter = Arc::new(wg::AsyncWaitGroup::new());
        let preparer = Arc::new(SkipPreparer::new());
        let gateway = Gateway::new(router.clone());
        waiter.add(1);
        Self { name, dir, router, handler, preparer, channels: HashMap::new(), interval: 3000, gateway, listener: HashMap::new(), waiter }
    }

    pub async fn bootstrap(handler: Arc<dyn Handler + Send + Sync>, config: Arc<JSON>) -> tokio::io::Result<Self> {
        let name = Arc::new(json_must_str(&config, "name")?.to_string());
        let mut proxy = Self::new(name, handler);
        proxy.dir = Arc::new(json_option_str(&config, "dir").unwrap_or(&String::from(".")).to_string());
        proxy.reset(config, true, true, true).await?;
        Ok(proxy)
    }

    pub async fn reset(&mut self, config: Arc<JSON>, channel: bool, forward: bool, web: bool) -> tokio::io::Result<()> {
        if channel {
            self.router.close_all().await;
            let channels = json_option_obj(&config, "channels")?;
            self.channels.clear();
            for name in channels.keys() {
                let channel = json_must_obj(&channels, name)?;
                self.channels.insert(name.clone(), channel);
            }
        }
        if forward {
            let forwards = json_option_obj(&config, "forwards")?;
            for loc in forwards.keys() {
                let parts: Vec<_> = loc.splitn(2, "~").collect();
                if parts.len() < 2 {
                    return Err(new_message_err(format!("{} is invalid", loc)));
                }
                let remote = json_must_str(&forwards, &loc)?;
                let name = Arc::new(parts[0].to_string());
                let loc = &parts[1].to_string();
                let remote = Arc::new(remote.to_string());
                self.start_forward(name, loc, remote).await?;
            }
        }
        if web {
            let web = json_option_obj(&config, "web")?;
            for name in web.keys() {
                let domain = json_must_str(&web, name)?;
                let name = Arc::new(name.clone());
                self.start_web(name, domain).await?;
            }
        }
        Ok(())
    }

    pub async fn run(&mut self, receiver: Receiver<u8>) {
        let mut interval = tokio::time::interval(Duration::from_millis(self.interval));
        let mut receiver = receiver;
        loop {
            tokio::select! {
                _= interval.tick() => {
                    _=self.keep().await;
                }
                _= receiver.recv() =>break,
            }
        }
    }

    pub async fn start_keep(proxy: Arc<Mutex<Proxy>>, ms: u64) {
        tokio::spawn(async move {
            Self::loop_keep(proxy, ms).await;
        });
    }

    async fn loop_keep(proxy: Arc<Mutex<Proxy>>, ms: u64) {
        let mut interval = tokio::time::interval(Duration::from_millis(ms));
        loop {
            interval.tick().await;
            _ = proxy.lock().await.keep().await;
        }
    }

    pub async fn keep(&mut self) -> tokio::io::Result<()> {
        let connected = self.router.list_channel_count().await;
        let mut to_login = Vec::new();
        let mut to_conn = Vec::new();
        for (name, option) in &self.channels {
            let keep = match json_option_i64(option, "keep") {
                Some(v) => v as usize,
                None => 1,
            };
            let count = match connected.get(name) {
                Some(c) => c.clone(),
                None => 0,
            };
            if count >= keep {
                continue;
            }
            to_login.push(option.clone());
            to_conn.push(keep - count);
        }
        for i in 0..to_login.len() {
            let option = &to_login[i];
            for _ in 0..to_conn[i] {
                match self.login(option.clone()).await {
                    Ok(_) => (),
                    Err(e) => {
                        warn!("Proxy({}) keep login by {:?} fail with {:?}", self.name, option, e);
                        break;
                    }
                }
            }
        }
        Ok(())
    }

    pub async fn login(&mut self, option: Arc<JSON>) -> tokio::io::Result<()> {
        let remote_all = json_must_str(&option, "remote")?;
        for remote in remote_all.split(",") {
            let remote = remote.to_string();
            if remote.starts_with("tcp://") {
                let conn = TcpSocket::new_v4()?;
                conn.bind("0.0.0.0:0".parse().unwrap())?;
                let fd = conn.as_raw_fd();
                self.preparer.prepare_fd(fd).await?;

                let domain = remote.trim_start_matches("tcp://");
                let addr = wrap_err(domain.parse())?;
                let stream = conn.connect(addr).await?;
                let (rx, tx) = wrap_split_tcp_w(stream);
                self.router.join_base(rx, tx, option.clone()).await?;
            } else if remote.starts_with("tls://") {
                let conn = TcpSocket::new_v4()?;
                conn.bind("0.0.0.0:0".parse().unwrap())?;
                let fd = conn.as_raw_fd();
                self.preparer.prepare_fd(fd).await?;

                let addr = remote.trim_start_matches("tls://").to_string();
                let domain = json_option_str(&option, "domain").unwrap_or(&addr);
                let addr_conn = wrap_err(addr.parse())?;
                let tls = load_tls_config(self.dir.clone(), &option)?;
                let connector = TlsConnector::from(tls);
                let stream = conn.connect(addr_conn).await?;
                let server_name = rustls::ServerName::try_from(domain.as_str()).map_err(|_| new_message_err("invalid domain"))?;
                let stream = connector.connect(server_name, stream).await?;
                let (rx, tx) = wrap_split_tls_w(stream);
                self.router.join_base(rx, tx, option.clone()).await?;
            } else if remote.starts_with("quic://") {
                let conn = std::net::UdpSocket::bind("0.0.0.0:0")?;
                let fd = conn.as_raw_fd();
                self.preparer.prepare_fd(fd).await?;

                let addr = remote.trim_start_matches("quic://").to_string();
                let domain: &String = json_option_str(&option, "domain").unwrap_or(&addr);
                let addr_conn = wrap_err(addr.parse())?;
                let runtime = quinn::default_runtime().ok_or_else(|| new_message_err("no async runtime found"))?;
                let mut endpoint = quinn::Endpoint::new(quinn::EndpointConfig::default(), None, conn, runtime)?;
                let tls = load_tls_config(self.dir.clone(), &option)?;
                endpoint.set_default_client_config(quinn::ClientConfig::new(tls));
                let conn = wrap_err(endpoint.connect(addr_conn, domain))?.await?;
                let (send, recv) = conn.open_bi().await?;
                let (rx, tx) = wrap_quinn_w(send, recv);
                self.router.join_base(rx, tx, option.clone()).await?;
            } else {
                return Err(new_message_err(format!("not supporeted {}", remote)));
            }
        }
        Ok(())
    }

    pub async fn load_state(&self, name: Arc<String>) -> Option<ServerState> {
        match self.listener.get(name.as_ref()) {
            Some(v) => Some(v.lock().await.clone()),
            None => None,
        }
    }

    pub async fn start_forward(&mut self, key: Arc<String>, loc: &String, remote: Arc<String>) -> tokio::io::Result<SocketAddr> {
        let name = self.name.clone();
        info!("Proxy({}) {} start forward by{}=>{}", name, key, loc, remote);
        let router = self.router.clone();
        if loc.starts_with("socks://") {
            let domain: &str = loc.trim_start_matches("socks://");
            let ln = TcpListener::bind(&domain).await?;
            let addr: SocketAddr = ln.local_addr().unwrap();
            let state = Arc::new(Mutex::new(ServerState::new(name.clone(), addr.to_string(), addr)));
            info!("Proxy({}) {} listen socks {} is success", name, key, addr);
            self.listener.insert(key.to_string(), state.clone());
            let waiter = self.waiter.clone();
            waiter.add(1);
            tokio::spawn(async move { Self::loop_socks_accpet(name, key, waiter, ln, state, router, remote).await });
            Ok(addr)
        } else if loc.starts_with("gw://") {
            let parts: Vec<_> = loc.trim_start_matches("gw://").splitn(2, ">").collect();
            let domain = parts[0].to_string();
            let sendto = parts[1].to_string();
            let ln_reader = WrapUdpConn::bind(domain.clone(), sendto.clone()).await?;
            let ln_writer = ln_reader.clone();
            let addr: SocketAddr = ln_reader.local_addr().unwrap();
            info!("Proxy({}) {} listen gw {}<=>{} is success", name, key, domain, sendto);
            let mut device = CacheDevice::new(1600);
            let mut config = Config::new(smoltcp::wire::HardwareAddress::Ip);
            config.random_seed = rand::random();
            let mut iface = Interface::new(config, &mut device);
            iface.update_ip_addrs(|ip_addrs| {
                ip_addrs.push(IpCidr::new(IpAddress::v4(10, 1, 0, 2), 24)).unwrap();
            });
            iface.set_any_ip(true);
            self.gateway.start(self.name.clone(), iface, ln_reader, ln_writer, remote).await;
            Ok(addr)
        } else {
            let domain: &str = loc.trim_start_matches("tcp://");
            let ln = TcpListener::bind(&domain).await?;
            let addr: SocketAddr = ln.local_addr().unwrap();
            let state = Arc::new(Mutex::new(ServerState::new(name.clone(), addr.to_string(), addr)));
            info!("Proxy({}) {} listen tcp {} is success", name, key, addr);
            self.listener.insert(key.to_string(), state.clone());
            let waiter = self.waiter.clone();
            waiter.add(1);
            tokio::spawn(async move { Self::loop_tcp_accpet(name, key, waiter, ln, state, router, remote).await });
            Ok(addr)
        }
    }

    pub async fn start_gateway<R, W>(&mut self, ip: IpCidr, reader: R, writer: W, remote: Arc<String>)
    where
        R: frame::RawReader + Send + Sync + 'static,
        W: frame::RawWriter + Send + Sync + 'static,
    {
        let mut device = CacheDevice::new(1600);
        let mut config = Config::new(smoltcp::wire::HardwareAddress::Ip);
        config.random_seed = rand::random();
        let mut iface = Interface::new(config, &mut device);
        iface.update_ip_addrs(|ip_addrs| ip_addrs.push(ip).unwrap());
        iface.set_any_ip(true);
        self.gateway.start(self.name.clone(), iface, reader, writer, remote).await;
    }

    async fn loop_tcp_accpet(name: Arc<String>, key: Arc<String>, waiter: Arc<wg::AsyncWaitGroup>, ln: TcpListener, state: Arc<Mutex<ServerState>>, router: Arc<Router>, remote: Arc<String>) -> tokio::io::Result<()> {
        waiter.add(1);
        info!("Proxy({}) {} forward tcp {:?}->{} loop is starting", name, key, ln.local_addr().unwrap(), &remote);
        let err = loop {
            match ln.accept().await {
                Ok((stream, from)) => {
                    if state.lock().await.stopping {
                        break new_message_err("stopped");
                    }
                    Self::proc_tcp_conn(&name, router.clone(), stream, from, remote.clone()).await;
                }
                Err(e) => {
                    warn!("Proxy({}) {} accept tcp on {:?} is fail by {:?}", name, key, ln.local_addr().unwrap(), e);
                }
            }
        };
        info!("Proxy({}) {} forward tcp {:?}->{} loop is stopped by {:?}", name, key, ln.local_addr().unwrap(), &remote, err);
        waiter.done();
        Ok(())
    }

    async fn proc_tcp_conn(name: &Arc<String>, router: Arc<Router>, stream: TcpStream, from: SocketAddr, remote: Arc<String>) {
        info!("Proxy({}) start forward tcp conn {:?} to {:?}", name, from, &remote);
        let (reader, writer) = wrap_split_tcp_w(stream);
        match router.dial_base(reader, writer, remote.clone()).await {
            Ok(_) => (),
            Err(e) => {
                info!("Proxy({}) forward tcp conn {:?} to {:?} fail with {:?}", name, from, &remote, e);
            }
        }
    }

    async fn loop_socks_accpet(name: Arc<String>, key: Arc<String>, waiter: Arc<wg::AsyncWaitGroup>, ln: TcpListener, state: Arc<Mutex<ServerState>>, router: Arc<Router>, remote: Arc<String>) -> tokio::io::Result<()> {
        info!("Proxy({}) {} forward socks5 {:?}->{} loop is starting", name, key, ln.local_addr().unwrap(), remote);
        let err = loop {
            match ln.accept().await {
                Ok((stream, from)) => {
                    info!("accept sockes proxy from {:?}", from);
                    if state.lock().await.stopping {
                        break new_message_err("stopped");
                    }
                    let name = name.clone();
                    let router = router.clone();
                    let remote = remote.clone();
                    let waiter = waiter.clone();
                    _ = tokio::spawn(async move {
                        waiter.add(1);
                        Self::proc_socks_conn(name, router, stream, from, remote).await;
                        waiter.done();
                    });
                }
                Err(e) => {
                    warn!("Proxy({}) {} accept socks5 on {:?} is fail by {:?}", name, key, ln.local_addr().unwrap(), e);
                }
            }
        };
        info!("Proxy({}) {} forward socks5 {:?}->{} loop is stopped by {:?}", name, key, ln.local_addr().unwrap(), &remote, err);
        waiter.done();
        Ok(())
    }

    async fn proc_socks_conn(name: Arc<String>, router: Arc<Router>, stream: TcpStream, from: SocketAddr, remote: Arc<String>) {
        info!("Proxy({}) start forward socks conn {:?} to {:?}", name, from, &remote);
        let (reader, writer) = wrap_split_tcp_w(stream);
        match router.dial_socks(reader, writer, remote.clone()).await {
            Ok(_) => (),
            Err(e) => {
                info!("Proxy({}) forward socks conn {:?} to {:?} fail with {:?}", name, from, &remote, e);
            }
        }
    }

    pub async fn start_web(&mut self, key: Arc<String>, domain: &String) -> tokio::io::Result<()> {
        let router = self.router.clone();
        let domain: &str = domain.trim_start_matches("tcp://");
        let ln = TcpListener::bind(&domain).await?;
        let addr = ln.local_addr().unwrap();
        let state = Arc::new(Mutex::new(ServerState::new(key.clone(), domain.to_string(), addr)));
        info!("Proxy({}) {} listen web server {} is success", self.name, key, domain);
        self.listener.insert(key.to_string(), state.clone());
        let name = self.name.clone();
        let waiter = self.waiter.clone();
        waiter.add(1);
        tokio::spawn(async move { Self::loop_web_accpet(name, key, waiter, ln, state, router).await });
        Ok(())
    }

    async fn loop_web_accpet(name: Arc<String>, key: Arc<String>, waiter: Arc<wg::AsyncWaitGroup>, ln: TcpListener, state: Arc<Mutex<ServerState>>, router: Arc<Router>) -> tokio::io::Result<()> {
        info!("Proxy({}) {} web server {} loop is starting", name, key, ln.local_addr().unwrap());
        let err = loop {
            let (stream, _) = ln.accept().await?;
            if state.lock().await.stopping {
                break new_message_err("stopped");
            }
            let name = name.clone();
            let key = key.clone();
            let router = router.clone();
            tokio::task::spawn(async move {
                let handler = ProxyWebHandler { router };
                if let Err(e) = http1::Builder::new().keep_alive(true).serve_connection(stream, handler).await {
                    warn!("Proxy({}) {} web server proc http fail with {:?}", name, key, e);
                }
            });
        };
        info!("Proxy({}) {} web server {} loop is stopped by {:?}", name, key, ln.local_addr().unwrap(), err);
        waiter.done();
        Ok(())
    }

    pub async fn wait(&self) {
        self.router.wait().await;
        self.waiter.clone().wait().await;
    }

    pub async fn shutdown(&mut self) {
        info!("Proxy({}) is stopping", self.name);
        for server in self.listener.values() {
            let mut server = server.lock().await;
            info!("Proxy({}) listener {} is stopping", self.name, server.to_string());
            server.stop().await;
        }
        self.gateway.stop().await;
        self.router.shutdown().await;
        self.gateway.wait().await;
        self.waiter.done();
        self.waiter.wait().await;
    }

    pub async fn display(&self) -> JSON {
        let mut info = JSON::new();
        info.insert("name".to_string(), json!(self.name.to_string()));
        info.insert("dir".to_string(), json!(self.dir.to_string()));
        let router = self.router.display().await;
        info.insert("router".to_string(), json!(router));
        let mut channels = JSON::new();
        for (k, v) in &self.channels {
            channels.insert(k.to_string(), json!((**v).clone()));
        }
        info.insert("channels".to_string(), json!(router));
        info
    }
}

struct ProxyWebHandler {
    pub router: Arc<Router>,
}

impl ProxyWebHandler {
    async fn display(router: Arc<Router>) -> Result<Response<Full<Bytes>>, hyper::Error> {
        let display = router.display().await;
        match serde_json::to_string(&display) {
            Ok(v) => Self::make_response(StatusCode::OK, v),
            Err(e) => Self::make_response(StatusCode::SERVICE_UNAVAILABLE, format!(r#"{{"code":-1,"message":"{}"}}"#, e.to_string())),
        }
    }

    async fn backtrace(_: Arc<Router>) -> Result<Response<Full<Bytes>>, hyper::Error> {
        Self::make_response(StatusCode::OK, format!("{:?}", std::backtrace::Backtrace::capture()))
    }

    fn make_response(code: StatusCode, s: String) -> Result<Response<Full<Bytes>>, hyper::Error> {
        Ok(Response::builder().status(code).body(Full::new(Bytes::from(s))).unwrap())
    }
}

impl Service<Request<Incoming>> for ProxyWebHandler {
    type Response = Response<Full<Bytes>>;
    type Error = hyper::Error;
    type Future = Pin<Box<dyn Future<Output = Result<Self::Response, Self::Error>> + Send>>;

    fn call(&mut self, req: Request<Incoming>) -> Self::Future {
        let uri = req.uri().path().to_string();
        let router = self.router.clone();
        Box::pin(async move {
            match uri.as_str() {
                "/display" => Self::display(router).await,
                "/backtrace" => Self::backtrace(router).await,
                _ => Self::make_response(StatusCode::NOT_FOUND, format!("{} NOT FOUND", uri)),
            }
        })
    }
}
