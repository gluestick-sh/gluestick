//! Schannel-backed transport for ureq 3.
//!
//! Replaces ureq's built-in `native-tls` feature (which pulls the heavy
//! `webpki-root-certs` root-cert table into `glue.exe`) with a bespoke
//! [`Connector`]/[`Transport`] that performs TLS via the [`native_tls`] crate.
//! On Windows `native-tls` uses the OS schannel root store, so no bundled
//! root certificates are needed — this is what keeps the release binary small.
//!
//! The agent is assembled with [`Agent::with_parts`] in
//! [`crate::downloader::Downloader::build_agent`].

use std::io::{self, Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Arc, RwLock};
use std::time::Duration;

use http::uri::Scheme;
use native_tls::TlsConnector as NativeTlsConnector;
use ureq::unversioned::transport::{
    Buffers, ConnectionDetails, Connector, LazyBuffers, NextTimeout, Transport,
};
use ureq::Error;

use crate::proxy::{base64_encode, ProxySettings, ProxySpec};

/// Input/output buffer sizes, mirroring ureq's tuned defaults.
const INPUT_BUFFER_SIZE: usize = 128 * 1024;
const OUTPUT_BUFFER_SIZE: usize = 128 * 1024;

/// Default connect/read/write timeout when `NextTimeout` is unset.
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// Upper bound on a proxy CONNECT response's header block.
const MAX_PROXY_HEADER_BYTES: usize = 16 * 1024;

/// Connector that opens a TCP socket and, for `https`, wraps it in TLS via
/// native-tls (schannel on Windows — OS root store, no bundled certs).
///
/// When a proxy is configured the socket goes to the proxy, a `CONNECT`
/// tunnel is established, and the TLS handshake for an `https` target runs
/// *inside* that tunnel (TLS to the target, not to the proxy).
#[derive(Debug)]
pub struct SchannelConnector {
    tls: NativeTlsConnector,
    /// Shared so the downloader can add package hosts after the agent was
    /// built (`proxy_packages`): the decision is made per connection.
    proxy: Arc<RwLock<ProxySettings>>,
}

impl SchannelConnector {
    /// Connector with explicit proxy settings.
    pub fn new(proxy: ProxySettings) -> Self {
        Self::with_shared(Arc::new(RwLock::new(proxy)))
    }

    /// Connector sharing settings that may change between connections.
    pub fn with_shared(proxy: Arc<RwLock<ProxySettings>>) -> Self {
        // Schannel is part of Windows; native-tls always initialises here.
        Self {
            tls: NativeTlsConnector::new().expect("schannel TLS is unavailable on this host"),
            proxy,
        }
    }
}

impl Default for SchannelConnector {
    fn default() -> Self {
        Self::new(ProxySettings::default())
    }
}

impl Connector<()> for SchannelConnector {
    type Out = SchannelTransport;

    fn connect(
        &self,
        details: &ConnectionDetails,
        _: Option<()>,
    ) -> Result<Option<Self::Out>, Error> {
        let uri = details.uri;
        let is_tls = uri.scheme() == Some(&Scheme::HTTPS);
        let host = uri.host().ok_or_else(|| {
            Error::Io(io::Error::new(io::ErrorKind::InvalidInput, "missing host"))
        })?;

        let spec = {
            let proxy = self
                .proxy
                .read()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            proxy
                .for_scheme(is_tls)
                .filter(|_| proxy.allows_proxy(host))
                .filter(|_| !proxy.bypasses(host))
                .cloned()
        };

        if let Some(spec) = spec {
            return self
                .connect_via_proxy(uri, host, is_tls, &spec, details.timeout)
                .map(Some);
        }

        let addrs = &details.addrs;
        if addrs.is_empty() {
            return Err(Error::ConnectionFailed);
        }

        let mut last_err: Option<Error> = None;
        for addr in addrs.iter() {
            match self.connect_addr(uri, *addr, is_tls, details.timeout) {
                Ok(transport) => return Ok(Some(transport)),
                Err(e) => last_err = Some(e),
            }
        }
        Err(last_err.unwrap_or(Error::ConnectionFailed))
    }
}

impl SchannelConnector {
    /// Map a native-tls failure onto ureq's `Tls` error, keeping the
    /// permanent/transient distinction the downloader relies on.
    fn tls_error(err: impl std::fmt::Display) -> Error {
        let detail = err.to_string().to_lowercase();
        let permanent = detail.contains("cert")
            || detail.contains("hostname")
            || detail.contains("verify")
            || detail.contains("trust")
            || detail.contains("unknown ca");
        if permanent {
            Error::Tls("schannel certificate validation failed")
        } else {
            Error::Tls("schannel handshake failed")
        }
    }

    /// Blocking TLS handshake on `tcp`, with the connect budget as the
    /// deadline (per-request timeouts are applied by the transport later).
    fn wrap_tls(
        &self,
        host: &str,
        tcp: TcpStream,
        per_addr: Duration,
    ) -> Result<InnerStream, Error> {
        // Without a deadline, a server that accepts TCP and then goes silent
        // hangs the client forever.
        tcp.set_read_timeout(Some(per_addr)).map_err(Error::Io)?;
        tcp.set_write_timeout(Some(per_addr)).map_err(Error::Io)?;
        let tls_stream = self.tls.connect(host, tcp).map_err(Self::tls_error)?;
        Ok(InnerStream::Tls(Box::new(tls_stream)))
    }

    fn transport(stream: InnerStream, is_tls: bool) -> SchannelTransport {
        SchannelTransport::new(
            stream,
            LazyBuffers::new(INPUT_BUFFER_SIZE, OUTPUT_BUFFER_SIZE),
            is_tls,
        )
    }

    fn connect_addr(
        &self,
        uri: &http::Uri,
        addr: std::net::SocketAddr,
        is_tls: bool,
        timeout: NextTimeout,
    ) -> Result<SchannelTransport, Error> {
        let per_addr = timeout.not_zero().map(|d| *d).unwrap_or(DEFAULT_TIMEOUT);
        let tcp = TcpStream::connect_timeout(&addr, per_addr).map_err(Error::Io)?;
        let _ = tcp.set_nonblocking(false);

        let host = uri.host().ok_or_else(|| {
            Error::Io(io::Error::new(io::ErrorKind::InvalidInput, "missing host"))
        })?;

        let stream = if is_tls {
            self.wrap_tls(host, tcp, per_addr)?
        } else {
            InnerStream::Tcp(tcp)
        };
        Ok(Self::transport(stream, is_tls))
    }

    /// Connect through an HTTP CONNECT proxy and, for `https` targets, run the
    /// TLS handshake to the target inside the tunnel.
    fn connect_via_proxy(
        &self,
        uri: &http::Uri,
        host: &str,
        is_tls: bool,
        spec: &ProxySpec,
        timeout: NextTimeout,
    ) -> Result<SchannelTransport, Error> {
        if spec.tls {
            return Err(Error::Io(io::Error::new(
                io::ErrorKind::InvalidInput,
                "https:// proxy URLs are not supported; configure an http:// CONNECT proxy",
            )));
        }
        let per_addr = timeout.not_zero().map(|d| *d).unwrap_or(DEFAULT_TIMEOUT);
        let target_port = uri.port_u16().unwrap_or(if is_tls { 443 } else { 80 });
        let addrs = (spec.host.as_str(), spec.port)
            .to_socket_addrs()
            .map_err(Error::Io)?;

        let mut last_err: Option<Error> = None;
        for addr in addrs {
            match Self::open_tunnel(addr, spec, host, target_port, per_addr) {
                Ok(tcp) => {
                    let stream = if is_tls {
                        self.wrap_tls(host, tcp, per_addr)?
                    } else {
                        InnerStream::Tcp(tcp)
                    };
                    return Ok(Self::transport(stream, is_tls));
                }
                Err(e) => last_err = Some(e),
            }
        }
        Err(last_err.unwrap_or(Error::ConnectionFailed))
    }

    /// Open a `CONNECT host:port` tunnel through the proxy at `addr`.
    fn open_tunnel(
        addr: std::net::SocketAddr,
        spec: &ProxySpec,
        target_host: &str,
        target_port: u16,
        timeout: Duration,
    ) -> Result<TcpStream, Error> {
        let mut tcp = TcpStream::connect_timeout(&addr, timeout).map_err(Error::Io)?;
        let _ = tcp.set_nonblocking(false);
        tcp.set_read_timeout(Some(timeout)).map_err(Error::Io)?;
        tcp.set_write_timeout(Some(timeout)).map_err(Error::Io)?;

        let mut request = format!(
            "CONNECT {target_host}:{target_port} HTTP/1.1\r\n\
             Host: {target_host}:{target_port}\r\n\
             User-Agent: Glue/1.0\r\n\
             Proxy-Connection: Keep-Alive\r\n"
        );
        if spec.username.is_some() || spec.password.is_some() {
            let credentials = format!(
                "{}:{}",
                spec.username.as_deref().unwrap_or(""),
                spec.password.as_deref().unwrap_or("")
            );
            request.push_str(&format!(
                "Proxy-Authorization: Basic {}\r\n",
                base64_encode(credentials.as_bytes())
            ));
        }
        request.push_str("\r\n");
        tcp.write_all(request.as_bytes()).map_err(Error::Io)?;
        tcp.flush().map_err(Error::Io)?;

        // Byte-at-a-time so the tunnel's first payload bytes are not consumed
        // together with the proxy's response headers.
        let mut response = Vec::with_capacity(128);
        let mut byte = [0u8; 1];
        loop {
            match tcp.read(&mut byte) {
                Ok(0) => {
                    return Err(Error::ConnectProxyFailed(
                        "proxy closed the connection during CONNECT".to_string(),
                    ))
                }
                Ok(_) => response.push(byte[0]),
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) => return Err(Error::Io(e)),
            }
            if response.ends_with(b"\r\n\r\n") {
                break;
            }
            if response.len() > MAX_PROXY_HEADER_BYTES {
                return Err(Error::ConnectProxyFailed(
                    "proxy response headers too large".to_string(),
                ));
            }
        }

        let text = String::from_utf8_lossy(&response);
        let status = text
            .lines()
            .next()
            .and_then(|line| line.split_whitespace().nth(1))
            .and_then(|code| code.parse::<u16>().ok())
            .unwrap_or(0);
        match status {
            200..=299 => Ok(tcp),
            407 => Err(Error::ConnectProxyFailed(
                "proxy requires authentication (407)".to_string(),
            )),
            other => Err(Error::ConnectProxyFailed(format!(
                "proxy responded {other}"
            ))),
        }
    }
}
/// Either a plain TCP socket or a native-tls (schannel) session on top of one.
#[derive(Debug)]
enum InnerStream {
    Tcp(TcpStream),
    Tls(Box<native_tls::TlsStream<TcpStream>>),
}

impl InnerStream {
    fn set_read_timeout(&mut self, d: Option<Duration>) -> io::Result<()> {
        match self {
            InnerStream::Tcp(s) => s.set_read_timeout(d),
            InnerStream::Tls(s) => s.get_mut().set_read_timeout(d),
        }
    }

    fn set_write_timeout(&mut self, d: Option<Duration>) -> io::Result<()> {
        match self {
            InnerStream::Tcp(s) => s.set_write_timeout(d),
            InnerStream::Tls(s) => s.get_mut().set_write_timeout(d),
        }
    }

    fn peek(&self, buf: &mut [u8]) -> io::Result<usize> {
        match self {
            InnerStream::Tcp(s) => s.peek(buf),
            InnerStream::Tls(s) => s.get_ref().peek(buf),
        }
    }
}

impl Read for InnerStream {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match self {
            InnerStream::Tcp(s) => s.read(buf),
            InnerStream::Tls(s) => s.read(buf),
        }
    }
}

impl Write for InnerStream {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        match self {
            InnerStream::Tcp(s) => s.write(buf),
            InnerStream::Tls(s) => s.write(buf),
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        match self {
            InnerStream::Tcp(s) => s.flush(),
            InnerStream::Tls(s) => s.flush(),
        }
    }
}

/// Transport over a TCP socket, optionally wrapped in native-tls (schannel).
#[derive(Debug)]
pub struct SchannelTransport {
    stream: InnerStream,
    buffers: LazyBuffers,
    is_tls: bool,
    /// Set when a read returned EOF; `is_open` must not peek a TLS socket.
    eof: bool,
}

impl SchannelTransport {
    fn new(stream: InnerStream, buffers: LazyBuffers, is_tls: bool) -> Self {
        Self {
            stream,
            buffers,
            is_tls,
            eof: false,
        }
    }
}

impl Transport for SchannelTransport {
    fn buffers(&mut self) -> &mut dyn Buffers {
        &mut self.buffers
    }

    fn transmit_output(&mut self, amount: usize, timeout: NextTimeout) -> Result<(), Error> {
        let _ = self.stream.set_write_timeout(Some(*timeout.after));
        let buf = &self.buffers.output()[..amount];
        let mut written = 0;
        while written < buf.len() {
            match self.stream.write(&buf[written..]) {
                Ok(0) => {
                    return Err(Error::Io(io::Error::new(
                        io::ErrorKind::WriteZero,
                        "transport write returned zero",
                    )))
                }
                Ok(n) => written += n,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) => return Err(Error::Io(e)),
            }
        }
        Ok(())
    }

    fn await_input(&mut self, timeout: NextTimeout) -> Result<bool, Error> {
        let _ = self.stream.set_read_timeout(Some(*timeout.after));
        let buf = self.buffers.input_append_buf();
        match self.stream.read(buf) {
            Ok(0) => {
                self.eof = true;
                Ok(false)
            }
            Ok(n) => {
                self.buffers.input_appended(n);
                Ok(n > 0)
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => Ok(false),
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => Ok(false),
            Err(e) => Err(Error::Io(e)),
        }
    }

    fn is_open(&mut self) -> bool {
        // Never peek a TLS socket: the underlying TCP stream carries
        // ciphertext, so a peek either blocks until the read timeout (measured
        // ~30 s on GitHub connections) or sees bytes that are not application
        // data. EOF is tracked by `await_input` instead.
        if self.is_tls {
            return !self.eof;
        }
        match self.stream.peek(&mut [0; 1]) {
            // A 0-byte peek is an orderly shutdown: the connection is dead
            // and must not be reused from the pool.
            Ok(0) => false,
            Err(ref e) if e.kind() == io::ErrorKind::WouldBlock => true,
            Ok(_) => true,
            Err(_) => false,
        }
    }

    fn is_tls(&self) -> bool {
        self.is_tls
    }
}
