use std::{borrow::Cow, collections::HashSet};

pub const CONFLICT_REASON: &str = "Request contains duplicate or ambiguous fields";

struct Container<'a> {
    names: Option<HashSet<Cow<'a, str>>>,
    freeform: bool,
}

pub fn has_conflict(body: &[u8]) -> bool {
    let body = body.strip_prefix(b"\xef\xbb\xbf").unwrap_or(body);
    let mut cursor = 0;
    skip_space(body, &mut cursor);
    if !matches!(body.get(cursor), Some(b'{' | b'[')) {
        return false;
    }
    let mut containers: Vec<Container<'_>> = Vec::new();
    let mut next_freeform = false;
    while let Some(&byte) = body.get(cursor) {
        match byte {
            b'{' | b'[' => {
                if containers.len() == 10_000 {
                    return true;
                }
                containers.push(Container {
                    names: (byte == b'{').then(HashSet::new),
                    freeform: next_freeform,
                });
            }
            b'}' | b']' => {
                containers.pop();
                if containers.is_empty() {
                    break;
                }
            }
            b',' => next_freeform = containers.last().is_some_and(|scope| scope.freeform),
            b'"' => {
                let start = cursor;
                cursor += 1;
                let mut escaped = false;
                while let Some(&byte) = body.get(cursor) {
                    match byte {
                        b'"' => break,
                        b'\\' => {
                            escaped = true;
                            cursor += 2;
                        }
                        _ => cursor += 1,
                    }
                }
                if cursor >= body.len() {
                    break;
                }
                let end = cursor;
                let mut after = end + 1;
                skip_space(body, &mut after);
                if let Some(scope) = containers.last_mut() {
                    if let Some(names) = scope.names.as_mut() {
                        if body.get(after) == Some(&b':') {
                            let key = if escaped {
                                match serde_json::from_slice::<String>(&body[start..=end]) {
                                    Ok(key) => Cow::Owned(key),
                                    Err(_) => return false,
                                }
                            } else {
                                String::from_utf8_lossy(&body[start + 1..end])
                            };
                            let key = if scope.freeform { key } else { fold_key(key) };
                            next_freeform = scope.freeform || is_freeform_key(&key);
                            if !names.insert(key) {
                                return true;
                            }
                        }
                    }
                }
            }
            _ => {}
        }
        cursor += 1;
    }
    false
}

fn skip_space(body: &[u8], cursor: &mut usize) {
    while body.get(*cursor).is_some_and(|byte| *byte <= b' ') {
        *cursor += 1;
    }
}

fn fold_key(key: Cow<'_, str>) -> Cow<'_, str> {
    if key.is_ascii() {
        if key.bytes().any(|byte| byte.is_ascii_uppercase()) {
            return Cow::Owned(key.to_ascii_lowercase());
        }
        return key;
    }
    Cow::Owned(
        key.chars()
            .map(|ch| match ch {
                'ſ' => 's',
                'ς' => 'σ',
                'µ' => 'μ',
                _ => ch.to_lowercase().next().unwrap_or(ch),
            })
            .collect(),
    )
}

fn is_freeform_key(key: &str) -> bool {
    matches!(
        key,
        "properties"
            | "patternproperties"
            | "definitions"
            | "$defs"
            | "dependentschemas"
            | "dependentrequired"
            | "dependencies"
            | "default"
            | "const"
            | "examples"
            | "enum"
            | "args"
            | "arguments"
            | "variables"
            | "headers"
            | "labels"
    )
}

pub fn requested_effort(body: &serde_json::Value) -> Option<&'static str> {
    let from_updates = body
        .get("input")
        .and_then(serde_json::Value::as_array)
        .and_then(|items| {
            items.iter().rev().find_map(|item| {
                if item.get("type").and_then(serde_json::Value::as_str)
                    != Some("configuration_update")
                {
                    return None;
                }
                effort_at(item, "/reasoning/effort")
            })
        });
    from_updates.or_else(|| {
        [
            "/reasoning/effort",
            "/reasoning_effort",
            "/output_config/effort",
        ]
        .iter()
        .find_map(|path| effort_at(body, path))
    })
}

fn effort_at(body: &serde_json::Value, path: &str) -> Option<&'static str> {
    let effort = body.pointer(path)?.as_str()?.trim().to_ascii_lowercase();
    match effort.as_str() {
        "none" => Some("none"),
        "minimal" => Some("minimal"),
        "low" => Some("low"),
        "medium" => Some("medium"),
        "high" => Some("high"),
        "xhigh" | "x-high" => Some("xhigh"),
        "max" => Some("max"),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn detects_conflicts_and_preserves_case_sensitive_schema_keys() {
        assert!(!has_conflict(
            br#"{"tools":[{"parameters":{"properties":{"A":{},"a":{}}}}]}"#
        ));
        assert!(has_conflict(br#"{"model":"name","MODEL":"name"}"#));
        assert!(has_conflict(
            br#"{"tools":[{"parameters":{"properties":{"a":{},"\u0061":{}}}}]}"#
        ));
        assert!(!has_conflict(
            br#"{"input":[{"type":"message"},{"type":"message"}]}"#
        ));
    }

    #[test]
    fn requested_effort_uses_latest_valid_update_then_the_root() {
        let body = json!({"reasoning":{"effort":"low"},"input":[
            {"type":"configuration_update","reasoning":{"effort":"high"}},
            {"type":"configuration_update","reasoning":{"effort":"unknown"}}
        ]});
        assert_eq!(requested_effort(&body), Some("high"));
        assert_eq!(
            requested_effort(&json!({"reasoning_effort":"low"})),
            Some("low")
        );
    }
}
