//! Version comparison helpers.

use std::cmp::Ordering;

/// Lowercase + trim + strip one leading `v`.
pub fn normalize_version(v: &str) -> String {
    let v = v.trim().to_lowercase();
    match v.strip_prefix('v') {
        Some(rest) => rest.to_string(),
        None => v,
    }
}

/// One parsed chunk: numeric (`text` empty) or textual (`num` 0), mirroring
#[derive(Debug, Clone, Default, PartialEq, Eq)]
struct VersionPart {
    num: i64,
    text: String,
}

fn is_int_like(s: &str) -> bool {
    s.parse::<i64>().is_ok()
}

/// Split a version into alternating numeric/text chunks
/// digits merge into a numeric chunk, any other
/// non-space rune starts a text chunk, and a digit after text flushes the
/// pending text.
fn parse_version_parts(v: &str) -> Vec<VersionPart> {
    let v = normalize_version(v);
    if v.is_empty() {
        return Vec::new();
    }
    let mut parts: Vec<VersionPart> = Vec::new();
    let mut buf = String::new();
    let flush = |parts: &mut Vec<VersionPart>, buf: &mut String| {
        if buf.is_empty() {
            return;
        }
        let s = std::mem::take(buf);
        if is_int_like(&s) {
            parts.push(VersionPart {
                num: s.parse::<i64>().unwrap(),
                text: String::new(),
            });
        } else {
            parts.push(VersionPart { num: 0, text: s });
        }
    };
    for c in v.chars() {
        if c.is_ascii_digit() {
            if !buf.is_empty() && !is_int_like(&buf) {
                flush(&mut parts, &mut buf);
            }
            buf.push(c);
        } else {
            flush(&mut parts, &mut buf);
            if !c.is_whitespace() {
                buf.push(c);
            }
        }
    }
    flush(&mut parts, &mut buf);
    parts
}

/// Compare two versions: `Less` when `a < b`, `Greater` when `a > b`
/// Numeric chunks compare numerically, text chunks
/// lexicographically, a missing chunk is treated as 0/empty, and equal
/// prefixes fall back to a plain string compare of the normalized forms.
pub fn version_compare(a: &str, b: &str) -> Ordering {
    let na = normalize_version(a);
    let nb = normalize_version(b);
    if na == nb {
        return Ordering::Equal;
    }
    let pa = parse_version_parts(&na);
    let pb = parse_version_parts(&nb);
    let n = pa.len().max(pb.len());
    for i in 0..n {
        let left = pa.get(i).cloned().unwrap_or_default();
        let right = pb.get(i).cloned().unwrap_or_default();
        if !left.text.is_empty() || !right.text.is_empty() {
            let ls = if left.text.is_empty() {
                left.num.to_string()
            } else {
                left.text.clone()
            };
            let rs = if right.text.is_empty() {
                right.num.to_string()
            } else {
                right.text.clone()
            };
            if ls != rs {
                return ls.cmp(&rs);
            }
            continue;
        }
        if left.num != right.num {
            return left.num.cmp(&right.num);
        }
    }
    na.cmp(&nb)
}

/// Whether `latest` is newer than `installed`.
pub fn update_available(installed: &str, latest: &str) -> bool {
    let installed = normalize_version(installed);
    let latest = normalize_version(latest);
    if installed.is_empty() || latest.is_empty() || installed == latest {
        return false;
    }
    version_compare(&latest, &installed) == Ordering::Greater
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn update_available_matrix() {
        // Engine/version_test.go TestUpdateAvailable.
        assert!(update_available("11.3.0", "11.4.0"));
        assert!(!update_available("11.4.0", "11.4.0"));
        assert!(!update_available("11.4.0", "11.3.0"));
        assert!(update_available("v1.0.0", "1.0.1"));
    }

    #[test]
    fn update_available_edges() {
        assert!(!update_available("", "1.0.0"));
        assert!(!update_available("1.0.0", ""));
        assert!(!update_available("", ""));
        assert!(!update_available("1.0.0", "  v1.0.0  "));
        // Missing chunk counts as zero: 1.0 and 1.0.0 tie.
        assert!(!update_available("1.0", "1.0.0"));
        // Pre-release-ish text chunks compare lexicographically.
        assert!(update_available("2.0.0-beta", "2.0.0"));
        assert!(update_available("1.9.9", "1.10.0"));
    }

    #[test]
    fn normalize_version_matrix() {
        assert_eq!(normalize_version("  V1.2.3 "), "1.2.3");
        assert_eq!(normalize_version("1.2.3"), "1.2.3");
        // Only one leading `v` is stripped (TrimPrefix semantics).
        assert_eq!(normalize_version("vv1"), "v1");
    }

    #[test]
    fn parse_version_parts_matrix() {
        let p = parse_version_parts("1a2");
        assert_eq!(p.len(), 3);
        assert_eq!(p[0].num, 1);
        assert_eq!(p[1].text, "a");
        assert_eq!(p[2].num, 2);
        // Each non-digit rune flushes first, so "beta" splits per-char
        // (parity).
        let p = parse_version_parts("2.0.0-beta.1");
        assert_eq!(p.len(), 12);
        assert_eq!(p[0].num, 2);
        assert_eq!(p[1].text, ".");
        assert_eq!(p[5].text, "-");
        assert_eq!(p[6].text, "b");
        assert_eq!(p[11].num, 1);
        assert_eq!(parse_version_parts(""), Vec::new());
    }

    #[test]
    fn version_compare_ordering() {
        assert_eq!(version_compare("1.0.0", "1.0.1"), Ordering::Less);
        assert_eq!(version_compare("1.0.1", "1.0.0"), Ordering::Greater);
        assert_eq!(version_compare("1.0.0", "1.0.0"), Ordering::Equal);
        assert_eq!(version_compare("v1.0", "1.0"), Ordering::Equal);
        assert_eq!(version_compare("1.10.0", "1.9.9"), Ordering::Greater);
        assert_eq!(version_compare("2.0.0-beta", "2.0.0"), Ordering::Less);
    }
}
