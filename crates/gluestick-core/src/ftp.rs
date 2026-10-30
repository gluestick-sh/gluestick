//! Minimal FTP client for `ftp://` download URLs.
//!
//! Uses `github.com/jlaffaye/ftp`; this is a from-scratch `std::net`
//! implementation covering exactly what the downloader needs: control
//! connection, anonymous/credentialed login, `SIZE`, and one `RETR` over a
//! PASV data connection. Error texts.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use url::Url;

use crate::downloader::{Progress, ProgressFn};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(60);
const IO_TIMEOUT: Duration = Duration::from_secs(60);
const COPY_BUFFER_SIZE: usize = 128 * 1024;

/// True when the URL scheme is `ftp` (case-insensitive) —.
pub fn is_ftp_url(raw: &str) -> bool {
    Url::parse(raw)
        .map(|u| u.scheme().eq_ignore_ascii_case("ftp"))
        .unwrap_or(false)
}

/// Host:port with FTP's default port 21.
fn dial_address(u: &Url) -> String {
    let host = u.host_str().unwrap_or("");
    match u.port() {
        Some(port) => format!("{host}:{port}"),
        None => format!("{host}:21"),
    }
}

/// Anonymous credentials unless the URL carries user info.
fn credentials(u: &Url) -> (String, String) {
    let user = u.username().to_string();
    let pass = u.password().unwrap_or("").to_string();
    if user.is_empty() {
        ("anonymous".to_string(), "anonymous@".to_string())
    } else if pass.is_empty() {
        (user, "anonymous@".to_string())
    } else {
        (user, pass)
    }
}

/// One FTP reply line (multi-line replies are folded into `text`).
struct Reply {
    code: u16,
    text: String,
}

/// FTP control connection (`*ftp.ServerConn` subset).
pub struct FtpClient {
    ctrl: BufReader<TcpStream>,
}

impl FtpClient {
    /// Dial and read the 220 greeting.
    pub fn connect(addr: &str) -> Result<FtpClient, String> {
        // Resolve manually so the TCP connect carries CONNECT_TIMEOUT too;
        // `TcpStream::connect` has no deadline and a black-holed server
        // would hang the download forever.
        let mut last: Option<String> = None;
        let mut stream = None;
        let addrs = addr
            .to_socket_addrs()
            .map_err(|e| format!("dial {addr}: {e}"))?;
        for sa in addrs {
            match TcpStream::connect_timeout(&sa, CONNECT_TIMEOUT) {
                Ok(s) => {
                    stream = Some(s);
                    break;
                }
                Err(e) => last = Some(format!("dial {addr}: {e}")),
            }
        }
        let stream =
            stream.ok_or_else(|| last.unwrap_or_else(|| format!("dial {addr}: no address")))?;
        stream
            .set_read_timeout(Some(IO_TIMEOUT))
            .map_err(|e| format!("dial {addr}: {e}"))?;
        stream
            .set_write_timeout(Some(IO_TIMEOUT))
            .map_err(|e| format!("dial {addr}: {e}"))?;
        let mut client = FtpClient {
            ctrl: BufReader::new(stream),
        };
        let reply = client.read_reply().map_err(|e| format!("greeting: {e}"))?;
        if reply.code != 220 {
            return Err(format!("greeting: {} {}", reply.code, reply.text));
        }
        Ok(client)
    }

    fn read_reply(&mut self) -> Result<Reply, String> {
        // Multi-line replies: "211-Features" ... "211 End". Only the final
        // line's payload matters for the commands used here.
        loop {
            let mut line = String::new();
            let n = self
                .ctrl
                .read_line(&mut line)
                .map_err(|e| format!("read reply: {e}"))?;
            if n == 0 {
                return Err("connection closed".to_string());
            }
            let trimmed = line.trim_end_matches(['\r', '\n']);
            if trimmed.len() < 4 {
                return Err(format!("malformed reply: {trimmed}"));
            }
            let (code, rest) = trimmed.split_at(3);
            let code: u16 = code
                .parse()
                .map_err(|_| format!("malformed reply: {trimmed}"))?;
            if rest.starts_with('-') {
                continue;
            }
            let text = rest.strip_prefix(' ').unwrap_or(rest).to_string();
            return Ok(Reply { code, text });
        }
    }

    fn command(&mut self, cmd: &str) -> Result<Reply, String> {
        // Never echo the command into errors: `PASS <password>` would leak
        // credentials into logs and error chains.
        const TAG: &str = "send command";
        let stream = self.ctrl.get_mut();
        stream
            .write_all(cmd.as_bytes())
            .map_err(|e| format!("{TAG}: {e}"))?;
        stream
            .write_all(b"\r\n")
            .map_err(|e| format!("{TAG}: {e}"))?;
        stream.flush().map_err(|e| format!("{TAG}: {e}"))?;
        self.read_reply()
    }

    /// USER/PASS handshake: 331 → PASS, expect 230/202.
    pub fn login(&mut self, user: &str, pass: &str) -> Result<(), String> {
        let reply = self
            .command(&format!("USER {user}"))
            .map_err(|e| format!("user: {e}"))?;
        if reply.code == 331 {
            let reply = self
                .command(&format!("PASS {pass}"))
                .map_err(|e| format!("pass: {e}"))?;
            if reply.code != 230 && reply.code != 202 {
                return Err(format!("login rejected: {} {}", reply.code, reply.text));
            }
            return Ok(());
        }
        if reply.code != 230 && reply.code != 202 {
            return Err(format!("login rejected: {} {}", reply.code, reply.text));
        }
        Ok(())
    }

    /// Binary transfer mode (default in the client too).
    pub fn set_binary(&mut self) -> Result<(), String> {
        let reply = self.command("TYPE I").map_err(|e| format!("type: {e}"))?;
        if reply.code != 200 {
            return Err(format!("type: {} {}", reply.code, reply.text));
        }
        Ok(())
    }

    /// File size in bytes, or `None` when unsupported.
    pub fn size(&mut self, path: &str) -> Option<u64> {
        let reply = self.command(&format!("SIZE {path}")).ok()?;
        if reply.code != 213 {
            return None;
        }
        reply.text.trim().parse::<u64>().ok().filter(|s| *s > 0)
    }

    /// Enter passive mode and open the data socket.
    fn pasv(&mut self) -> Result<TcpStream, String> {
        let reply = self.command("PASV").map_err(|e| format!("pasv: {e}"))?;
        if reply.code != 227 {
            return Err(format!("pasv: {} {}", reply.code, reply.text));
        }
        // "227 Entering Passive Mode (127,0,0,1,195,212)"
        let inner = reply
            .text
            .split_once('(')
            .and_then(|(_, rest)| rest.split_once(')'))
            .map(|(inner, _)| inner)
            .ok_or_else(|| format!("pasv: malformed reply: {}", reply.text))?;
        let nums: Vec<u64> = inner
            .split(',')
            .filter_map(|p| p.trim().parse().ok())
            .collect();
        if nums.len() != 6 || nums.iter().any(|n| *n > 255) {
            return Err(format!("pasv: malformed reply: {}", reply.text));
        }
        let port = (nums[4] * 256 + nums[5]) as u16;
        if port == 0 {
            return Err(format!("pasv: malformed reply: {}", reply.text));
        }
        // Deliberately ignore the IP in the 227 reply and reuse the control
        // connection's peer: the reply IP is fully server-controlled (and
        // injectable by a MITM on plaintext FTP), so honouring it would let
        // a server steer the data connection at arbitrary hosts — SSRF and
        // internal port scanning. Intentional divergence from
        // jlaffaye/ftp, which dials the given address; curl defaults to the
        // same behaviour as this implementation (--ftp-skip-pasv-ip), which also
        // tolerates NAT-broken servers that answer with a private IP.
        let peer = self
            .ctrl
            .get_ref()
            .peer_addr()
            .map_err(|e| format!("pasv: peer addr: {e}"))?;
        TcpStream::connect_timeout(&std::net::SocketAddr::new(peer.ip(), port), CONNECT_TIMEOUT)
            .map_err(|e| format!("pasv connect {}:{port}: {e}", peer.ip()))
    }

    /// Open a `RETR` stream: PASV data socket + 125/150 acknowledgement.
    fn retr(&mut self, path: &str) -> Result<TcpStream, String> {
        let data = self.pasv()?;
        let reply = self
            .command(&format!("RETR {path}"))
            .map_err(|e| format!("retr: {e}"))?;
        if reply.code != 125 && reply.code != 150 {
            return Err(format!("retr: {} {}", reply.code, reply.text));
        }
        Ok(data)
    }

    /// Read the transfer-complete reply (226/250) after the data socket EOF.
    pub fn finish_transfer(&mut self) -> Result<(), String> {
        let reply = self.read_reply().map_err(|e| format!("transfer: {e}"))?;
        if reply.code != 226 && reply.code != 250 {
            return Err(format!("transfer: {} {}", reply.code, reply.text));
        }
        Ok(())
    }

    /// Send `QUIT`; failures are intentionally ignored (`defer conn.Quit()`).
    pub fn quit(&mut self) {
        let _ = self.command("QUIT");
    }
}

#[derive(Debug, thiserror::Error)]
pub enum FtpError {
    #[error("{0}")]
    Ftp(String),
    #[error("write partial file: {0}")]
    WritePartial(#[source] std::io::Error),
    #[error("sync partial file: {0}")]
    SyncPartial(#[source] std::io::Error),
    #[error("incomplete ftp download: got {got} bytes, expected {expected}")]
    Incomplete { got: u64, expected: u64 },
}

/// Download an `ftp://` URL into the resumable `.part` file
/// Returns the total size; the caller writes the
/// sidecar meta.
pub fn download_ftp_to_partial(
    url_str: &str,
    part_path: &Path,
    filename: &str,
    progress: ProgressFn,
) -> Result<u64, FtpError> {
    let u = Url::parse(url_str).map_err(|e| FtpError::Ftp(format!("parse ftp URL: {e}")))?;
    let addr = dial_address(&u);
    let mut conn =
        FtpClient::connect(&addr).map_err(|e| FtpError::Ftp(format!("ftp connect: {e}")))?;
    let (user, pass) = credentials(&u);

    // Defers `conn.Quit()`; a closure keeps every exit path covered.
    let result = (|| {
        conn.login(&user, &pass)
            .map_err(|e| FtpError::Ftp(format!("ftp login: {e}")))?;

        let remote_path = u.path().to_string();
        if remote_path.is_empty() || remote_path == "/" {
            return Err(FtpError::Ftp("ftp URL has no file path".to_string()));
        }

        conn.set_binary()
            .map_err(|e| FtpError::Ftp(format!("ftp type: {e}")))?;

        let expected_size = conn.size(&remote_path).unwrap_or(0);

        let written = retr_into_file(
            &mut conn,
            &remote_path,
            part_path,
            filename,
            expected_size,
            progress,
        )?;

        if expected_size > 0 && written != expected_size {
            return Err(FtpError::Incomplete {
                got: written,
                expected: expected_size,
            });
        }
        Ok(if expected_size > 0 {
            expected_size
        } else {
            written
        })
    })();
    conn.quit();
    result
}

/// `RETR` the remote path straight into `part_path` (truncating), with
/// progress events; returns the number of bytes written.
fn retr_into_file(
    conn: &mut FtpClient,
    remote_path: &str,
    part_path: &Path,
    filename: &str,
    expected_size: u64,
    progress: ProgressFn,
) -> Result<u64, FtpError> {
    let mut data = conn
        .retr(remote_path)
        .map_err(|e| FtpError::Ftp(format!("ftp retr: {e}")))?;
    data.set_read_timeout(Some(IO_TIMEOUT)).ok();

    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(part_path)
        .map_err(FtpError::WritePartial)?;

    // Hoisted out of the read loop: the callback fires once per 128 KiB
    // read, and the filename must not cost an allocation per event.
    let fname: Arc<str> = Arc::from(filename);

    if let Some(cb) = progress {
        cb(&Progress {
            filename: fname.clone(),
            total: expected_size,
            offset: 0,
        });
    }

    let mut buf = [0u8; COPY_BUFFER_SIZE];
    let mut written: u64 = 0;
    loop {
        match data.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                if let Err(e) = file.write_all(&buf[..n]) {
                    let _ = file.sync_all();
                    return Err(FtpError::WritePartial(e));
                }
                written += n as u64;
                if let Some(cb) = progress {
                    cb(&Progress {
                        filename: fname.clone(),
                        total: expected_size,
                        offset: written,
                    });
                }
            }
            Err(e) => {
                let _ = file.sync_all();
                return Err(FtpError::Ftp(format!("ftp read: {e}")));
            }
        }
    }
    file.sync_all().map_err(FtpError::SyncPartial)?;
    // The 226 reply only arrives after the data socket closes.
    drop(data);
    conn.finish_transfer()
        .map_err(|e| FtpError::Ftp(format!("ftp transfer: {e}")))?;
    Ok(written)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn is_ftp_url_matches_go() {
        assert!(is_ftp_url(
            "ftp://download.tuxfamily.org/opendungeons/0.7/OpenDungeons-0.7.1-Win32-MSVS2013.zip"
        ));
        assert!(!is_ftp_url("https://example.com/file.zip"));
        assert!(!is_ftp_url("http://example.com/file.zip"));
        // Case-insensitive scheme (url.Parse + EqualFold).
        assert!(is_ftp_url("FTP://example.com/file.zip"));
        assert!(!is_ftp_url("example.com/file.zip"));
        assert!(!is_ftp_url(""));
    }

    #[test]
    fn dial_address_and_credentials() {
        let u = Url::parse("ftp://user:pw@ftp.example.com:2121/dir/file.bin").unwrap();
        assert_eq!(dial_address(&u), "ftp.example.com:2121");
        assert_eq!(credentials(&u), ("user".to_string(), "pw".to_string()));
        // Default port 21 + anonymous credentials.
        let u = Url::parse("ftp://ftp.example.com/dir/file.bin").unwrap();
        assert_eq!(dial_address(&u), "ftp.example.com:21");
        assert_eq!(
            credentials(&u),
            ("anonymous".to_string(), "anonymous@".to_string())
        );
        // User without password still gets anonymous@ as the password.
        let u = Url::parse("ftp://bob@ftp.example.com/dir/file.bin").unwrap();
        assert_eq!(
            credentials(&u),
            ("bob".to_string(), "anonymous@".to_string())
        );
    }

    #[test]
    fn pasv_ignores_reply_ip_and_uses_control_peer() {
        use std::net::TcpListener;

        // The data listener the client must reach even though the 227 reply
        // points at a different host.
        let data_listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let data_port = data_listener.local_addr().unwrap().port();
        let (p1, p2) = (data_port / 256, data_port % 256);

        let ctrl_listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let ctrl_port = ctrl_listener.local_addr().unwrap().port();
        let server = std::thread::spawn(move || {
            let (mut sock, _) = ctrl_listener.accept().unwrap();
            let mut reader = BufReader::new(sock.try_clone().unwrap());
            sock.write_all(b"220 ready\r\n").unwrap();
            let mut line = String::new();
            reader.read_line(&mut line).unwrap();
            assert!(line.starts_with("PASV"), "got {line:?}");
            // An "evil" internal IP: honouring it would steer the data
            // connection away from the control peer (SSRF).
            write!(sock, "227 Entering Passive Mode (10,0,0,9,{p1},{p2})\r\n").unwrap();
            sock.flush().unwrap();
            // The client must connect back to the control peer (127.0.0.1).
            let (_data, _) = data_listener.accept().unwrap();
        });

        let mut client = FtpClient::connect(&format!("127.0.0.1:{ctrl_port}")).unwrap();
        let data = client.pasv().unwrap();
        let peer = data.peer_addr().unwrap();
        assert_eq!(peer.ip().to_string(), "127.0.0.1");
        assert_eq!(peer.port(), data_port);
        drop(data);
        server.join().unwrap();
    }
}
