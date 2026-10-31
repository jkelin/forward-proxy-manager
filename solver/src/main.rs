//! Clears one Cloudflare challenge page for the Go proxy.
//!
//! Reads a JSON `Request` on stdin and writes one JSON `Answer` line on stdout. Exits 0 when the
//! challenge was cleared and 1 otherwise; progress and debug output are discarded. Credentials
//! travel on stdin so they never show up in the process list.

use std::io::{self, Read, Write};
use std::process::ExitCode;
use std::time::UNIX_EPOCH;

use cfsolve::{Browser, Clearance, Cookie, Error, Options};
use serde::{Deserialize, Serialize};
use url::Url;

/// What to clear and how to reach it.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    /// The challenged page; must be absolute http(s).
    url: Url,
    /// Upstream proxy, e.g. `socks5h://user:password@host:1080`. The clearance only works from
    /// this proxy's IP.
    proxy: String,
    /// cfsolve browser profile file; the caller must send later requests with the same identity.
    profile: String,
}

#[derive(Serialize)]
#[serde(tag = "outcome", rename_all = "kebab-case")]
enum Answer {
    Cleared {
        cookies: Vec<AnswerCookie>,
        page: AnswerPage,
    },
    Failed {
        kind: &'static str,
        message: String,
    },
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct AnswerCookie {
    name: String,
    value: String,
    domain: Option<String>,
    path: Option<String>,
    secure: bool,
    http_only: bool,
    /// Unix seconds; null for session cookies.
    expires: Option<u64>,
}

/// The page the challenge protected.
#[derive(Serialize)]
struct AnswerPage {
    url: String,
    status: u16,
    html: String,
}

impl AnswerCookie {
    fn from_cookie(cookie: &Cookie<'_>) -> Self {
        Self {
            name: cookie.name().to_owned(),
            value: cookie.value().to_owned(),
            domain: cookie.domain().map(str::to_owned),
            path: cookie.path().map(str::to_owned),
            secure: cookie.secure(),
            http_only: cookie.http_only(),
            // A pre-1970 expiry is already expired; 0 tells the caller to drop the cookie.
            expires: cookie.expires().map(|time| {
                time.duration_since(UNIX_EPOCH)
                    .map_or(0, |since| since.as_secs())
            }),
        }
    }
}

/// Stable failure kinds the Go side can branch on; the message keeps the details.
fn failure_kind(error: &Error) -> &'static str {
    match error {
        Error::Config(_) => "config",
        Error::Network(_) => "network",
        Error::Blocked { .. } => "blocked",
        Error::Challenged { .. } => "challenged",
        Error::NotChallenged => "not-challenged",
        Error::Rejected { .. } => "rejected",
        _ => "unsupported",
    }
}

fn cleared(clearance: Clearance) -> Answer {
    Answer::Cleared {
        cookies: clearance
            .cookies
            .iter()
            .map(AnswerCookie::from_cookie)
            .collect(),
        page: AnswerPage {
            url: clearance.page.url.to_string(),
            status: clearance.page.status.as_u16(),
            html: clearance.page.html,
        },
    }
}

fn failed(kind: &'static str, message: impl ToString) -> Answer {
    Answer::Failed {
        kind,
        message: message.to_string(),
    }
}

/// Edge cases: unreadable or invalid stdin and non-http(s) URLs fail as "config" before any
/// network access.
async fn run() -> Answer {
    let mut input = String::new();
    if let Err(error) = io::stdin().read_to_string(&mut input) {
        return failed("config", format!("reading stdin: {error}"));
    }

    let request: Request = match serde_json::from_str(&input) {
        Ok(request) => request,
        Err(error) => return failed("config", format!("invalid request: {error}")),
    };

    if !matches!(request.url.scheme(), "http" | "https") {
        return failed("config", format!("not an http(s) URL: {}", request.url));
    }

    let browser = match Browser::read(&request.profile) {
        Ok(browser) => browser,
        Err(error) => return failed(failure_kind(&error), error),
    };

    let options = Options {
        proxy: Some(request.proxy),
        ..Default::default()
    };

    match browser.clear(&request.url, &options).await {
        Ok(clearance) => cleared(clearance),
        Err(error) => failed(failure_kind(&error), format!("{error:#}")),
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    let answer = run().await;
    let code = match answer {
        Answer::Cleared { .. } => ExitCode::SUCCESS,
        Answer::Failed { .. } => ExitCode::FAILURE,
    };

    let line = serde_json::to_string(&answer).expect("answers serialize");
    let mut stdout = io::stdout().lock();
    if writeln!(stdout, "{line}")
        .and_then(|()| stdout.flush())
        .is_err()
    {
        return ExitCode::FAILURE;
    }

    code
}
