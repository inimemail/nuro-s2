//! Content-free timing observations. This module never changes relay output,
//! timeouts, usage, TTFT or retry decisions.
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{
    collections::HashSet,
    io::{self, Write},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct Request {
    #[serde(default)]
    pub has_tools: bool,
    pub effort: String,
    #[serde(default)]
    pub continuation: bool,
    pub input_bucket: usize,
    pub service_tier: String,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Sample {
    pub request: Request,
    pub model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub first_ready_ms: Option<i64>,
    #[serde(default)]
    pub max_gap_ms: i64,
    #[serde(default)]
    pub tool_gap_ms: i64,
    #[serde(default)]
    pub long_gaps: usize,
    #[serde(default)]
    pub long_wait_ms: i64,
    #[serde(default)]
    pub wait_ms: i64,
    #[serde(default)]
    pub complete: bool,
    #[serde(default)]
    pub at_ms: i64,
}

pub struct Tracker {
    sample: Option<Sample>,
    clock: Duration,
    last: Duration,
    progress: bool,
    terminal: bool,
    tools: HashSet<String>,
}

// Size cohorts need only a byte count, never another copy of the full request.
#[derive(Default)]
struct RequestSize(usize);
impl Write for RequestSize {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.0 = self.0.saturating_add(bytes.len());
        Ok(bytes.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[derive(Default)]
pub struct Session {
    effort: Option<String>,
    tier: Option<String>,
    tools: Option<bool>,
    image_tools: bool,
    image_choice: bool,
    compaction: bool,
    ambiguous: bool,
}
impl Session {
    pub fn update(&mut self, body: Option<&Value>) {
        let Some(session) = body
            .filter(|v| v.get("type").and_then(Value::as_str) == Some("session.update"))
            .and_then(|v| v.get("session"))
        else {
            return;
        };
        if let Some(effort) = session
            .pointer("/reasoning/effort")
            .or_else(|| session.get("reasoning_effort"))
        {
            self.effort = Some(
                match effort.as_str().unwrap_or("") {
                    "none" => "none",
                    "minimal" => "minimal",
                    "low" => "low",
                    "medium" => "medium",
                    "high" => "high",
                    "xhigh" => "xhigh",
                    "max" => "max",
                    _ => "unspecified",
                }
                .to_owned(),
            )
        }
        if let Some(tier) = session.get("service_tier") {
            self.tier = Some(
                match tier.as_str().unwrap_or("") {
                    "auto" => "auto",
                    "default" => "default",
                    "flex" => "flex",
                    "priority" => "priority",
                    _ => "unspecified",
                }
                .to_owned(),
            )
        }
        if let Some(tools) = session.get("tools") {
            self.tools = Some(tools.as_array().is_some_and(|a| !a.is_empty()));
            self.image_tools = crate::openai_json_tools_contain_image_generation(Some(tools));
        }
        if let Some(compaction) = session.get("context_management") {
            self.compaction = !compaction.is_null()
        }
        if let Some(choice) = session.get("tool_choice") {
            self.image_choice =
                crate::openai_json_tool_choice_selects_image_generation(Some(choice));
        }
    }
    pub fn tracker(&self, body: Option<&Value>, enabled: bool, model: Option<&str>) -> Tracker {
        let mut t = Tracker::from_body(body, enabled && !self.ambiguous, model);
        let Some(body) = body else { return t };
        if (body.get("tools").is_none() && self.image_tools)
            || (body.get("tool_choice").is_none() && self.image_choice)
            || (body.get("context_management").is_none() && self.compaction)
        {
            t.disable();
            return t;
        }
        if let Some(sample) = t.sample.as_mut() {
            if body.pointer("/reasoning/effort").is_none() && body.get("reasoning_effort").is_none()
            {
                if let Some(effort) = self.effort.as_ref() {
                    sample.request.effort = effort.clone()
                }
            }
            if body.get("service_tier").is_none() {
                if let Some(tier) = self.tier.as_ref() {
                    sample.request.service_tier = tier.clone()
                }
            }
            if body.get("tools").is_none() {
                if let Some(tools) = self.tools {
                    sample.request.has_tools = tools
                }
            }
        }
        t
    }
    pub fn begin_turn(
        &mut self,
        body: Option<&Value>,
        enabled: bool,
        model: Option<&str>,
        already_active: bool,
    ) -> Tracker {
        // Once turns overlap, terminal frames cannot be assigned reliably.
        // Keep this connection unscored even after one terminal arrives.
        self.ambiguous |= already_active;
        self.tracker(body, enabled, model)
    }
}
impl Tracker {
    pub fn new(template: Option<Sample>) -> Self {
        Self {
            sample: template.map(|s| Sample {
                request: s.request,
                model: s.model,
                ..Default::default()
            }),
            clock: Duration::ZERO,
            last: Duration::ZERO,
            progress: false,
            terminal: false,
            tools: HashSet::new(),
        }
    }
    pub fn active(&self) -> bool {
        self.sample.is_some() && !self.terminal
    }
    pub fn disable(&mut self) {
        self.sample = None;
    }
    pub fn from_body(body: Option<&Value>, enabled: bool, model: Option<&str>) -> Self {
        let Some(body) = body.filter(|_| enabled) else {
            return Self::new(None);
        };
        let model = body
            .get("model")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .or(model)
            .unwrap_or("");
        if model.is_empty() || model.len() > 200 {
            return Self::new(None);
        }
        let normalized = model.trim().to_ascii_lowercase();
        if normalized.starts_with("gpt-image-")
            || normalized == "grok-imagine"
            || normalized == "grok-imagine-edit"
            || normalized.starts_with("grok-imagine-image")
            || body.get("context_management").is_some()
            || crate::is_openai_ws_image_generation_intent(body)
        {
            return Self::new(None);
        }
        let effort = body
            .pointer("/reasoning/effort")
            .or_else(|| body.get("reasoning_effort"))
            .and_then(Value::as_str)
            .unwrap_or("unspecified");
        let effort = match effort {
            "none" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max" => effort,
            _ => "unspecified",
        };
        let tier = body
            .get("service_tier")
            .and_then(Value::as_str)
            .unwrap_or("unspecified");
        let tier = match tier {
            "auto" | "default" | "flex" | "priority" => tier,
            _ => "unspecified",
        };
        let mut size = RequestSize::default();
        if serde_json::to_writer(&mut size, body).is_err() {
            return Self::new(None);
        }
        let input_bucket = [16 << 10, 64 << 10, 256 << 10, 1 << 20]
            .iter()
            .filter(|n| size.0 > **n)
            .count();
        let continuation = body
            .get("previous_response_id")
            .and_then(Value::as_str)
            .is_some_and(|s| !s.is_empty());
        Self::new(Some(Sample {
            request: Request {
                has_tools: body
                    .get("tools")
                    .and_then(Value::as_array)
                    .is_some_and(|a| !a.is_empty()),
                effort: effort.to_owned(),
                continuation,
                input_bucket,
                service_tier: tier.to_owned(),
            },
            model: model.to_owned(),
            ..Default::default()
        }))
    }
    pub fn advance(&mut self, wait: Duration) {
        if self.active() {
            self.clock = self.clock.saturating_add(wait)
        }
    }
    pub fn observe(&mut self, value: &Value) {
        if !self.active() {
            return;
        }
        let kind = value.get("type").and_then(Value::as_str).unwrap_or("");
        let delta = value
            .get("delta")
            .and_then(Value::as_str)
            .is_some_and(|s| !s.trim().is_empty());
        let terminal = matches!(
            kind,
            "response.completed"
                | "response.done"
                | "response.failed"
                | "response.incomplete"
                | "response.cancelled"
                | "response.canceled"
        );
        let item_type = value
            .pointer("/item/type")
            .and_then(Value::as_str)
            .unwrap_or("");
        let tool_added = kind == "response.output_item.added"
            && matches!(item_type, "function_call" | "custom_tool_call");
        let semantic =
            (kind.ends_with(".delta") && kind != "response.transport_progress.delta" && delta)
                || tool_added
                || terminal;
        let sample = self.sample.as_mut().unwrap();
        if semantic {
            if self.progress {
                let gap = self.clock.saturating_sub(self.last).as_millis() as i64;
                sample.max_gap_ms = sample.max_gap_ms.max(gap);
                if !self.tools.is_empty() {
                    sample.tool_gap_ms = sample.tool_gap_ms.max(gap)
                }
                if gap >= 20_000 {
                    sample.long_gaps += 1;
                    sample.long_wait_ms += gap
                }
            }
            self.last = self.clock;
            self.progress = true;
        }
        if sample.first_ready_ms.is_none()
            && delta
            && matches!(
                kind,
                "response.output_text.delta"
                    | "response.function_call_arguments.delta"
                    | "response.custom_tool_call_input.delta"
            )
        {
            sample.first_ready_ms = Some(self.clock.as_millis() as i64)
        }
        if let Some(id) = value.pointer("/item/id").and_then(Value::as_str) {
            if tool_added && id.len() <= 256 && self.tools.len() < 128 {
                self.tools.insert(id.to_owned());
            }
            if kind == "response.output_item.done" {
                self.tools.remove(id);
            }
        }
        if terminal {
            self.terminal = true;
            sample.at_ms = timestamp_ms();
            sample.complete = matches!(kind, "response.completed" | "response.done")
                && value
                    .pointer("/response/status")
                    .and_then(Value::as_str)
                    .is_none_or(|s| s == "completed");
        }
    }
    pub fn observe_sse(&mut self, frame: &[u8]) {
        if !self.active() {
            return;
        }
        if frame.len() > 1 << 20 {
            self.disable();
            return;
        }
        let Ok(text) = std::str::from_utf8(frame) else {
            self.disable();
            return;
        };
        let mut data = String::new();
        for line in text.lines() {
            if let Some(part) = line.strip_prefix("data:") {
                if !data.is_empty() {
                    data.push('\n')
                }
                data.push_str(part.strip_prefix(' ').unwrap_or(part));
            }
        }
        if let Ok(value) = serde_json::from_str(&data) {
            self.observe(&value)
        }
    }
    pub fn snapshot(&self) -> Option<Sample> {
        self.sample.clone().map(|mut s| {
            s.wait_ms = self.clock.as_millis() as i64;
            if s.at_ms == 0 {
                s.at_ms = timestamp_ms();
            }
            s
        })
    }
}

fn timestamp_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as i64
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    fn tracker() -> Tracker {
        Tracker::from_body(
            Some(&json!({"model":"gpt-6.1-sol","reasoning":{"effort":"high"}})),
            true,
            None,
        )
    }
    #[test]
    fn explicit_images_and_unbounded_models_have_no_cohort() {
        for body in [
            json!({"model":"gpt-image-2"}),
            json!({"model":"gpt-test","tools":[{"type":" IMAGE_GENERATION "}]}),
            json!({"model":"gpt-test","tool_choice":{"type":"function","function":{"name":"image_gen.imagegen"}}}),
            json!({"model":"gpt-test","input":[{"type":"function_call","name":"image_gen.imagegen"}]}),
            json!({"model":"x".repeat(201)}),
        ] {
            assert!(Tracker::from_body(Some(&body), true, None)
                .snapshot()
                .is_none());
        }
        let passive = json!({"model":"gpt-test","tools":[{"type":"namespace","name":"image_gen"}]});
        assert!(Tracker::from_body(Some(&passive), true, None).active());
        let mut session = Session::default();
        session.update(Some(
            &json!({"type":"session.update","session":{"tool_choice":"image_generation"}}),
        ));
        assert!(session
            .tracker(Some(&passive), true, None)
            .snapshot()
            .is_none());
    }
    #[test]
    fn request_size_counter_matches_serialized_bytes() {
        for len in [0, 16_000, 65_000, 260_000, 1_050_000] {
            let body = json!({"model":"gpt-test","input":"\n\"你好".repeat(len)});
            let mut count = RequestSize::default();
            serde_json::to_writer(&mut count, &body).unwrap();
            let len = serde_json::to_vec(&body).unwrap().len();
            assert_eq!(count.0, len);
            let expected = [16 << 10, 64 << 10, 256 << 10, 1 << 20]
                .iter()
                .filter(|n| len > **n)
                .count();
            assert_eq!(
                Tracker::from_body(Some(&body), true, None)
                    .snapshot()
                    .unwrap()
                    .request
                    .input_bucket,
                expected
            );
        }
    }
    #[test]
    fn terminal_timestamp_does_not_refresh_after_delayed_completion() {
        let mut t = tracker();
        t.observe(&json!({"type":"response.completed"}));
        assert!(t.sample.as_ref().unwrap().at_ms > 0);
        let old = timestamp_ms() - 3_600_000;
        t.sample.as_mut().unwrap().at_ms = old;
        t.advance(Duration::from_secs(3600));
        t.observe(&json!({"type":"response.completed"}));
        let sample = t.snapshot().unwrap();
        assert_eq!(sample.at_ms, old);
        assert_eq!(sample.wait_ms, 0);
    }
    #[test]
    fn overlapping_turns_remain_unscored_for_the_session() {
        let mut s = Session::default();
        let body = json!({"model":"gpt-6.1-sol"});
        assert!(s.begin_turn(Some(&body), true, None, false).active());
        assert!(s
            .begin_turn(Some(&body), true, None, true)
            .snapshot()
            .is_none());
        assert!(s
            .begin_turn(Some(&body), true, None, false)
            .snapshot()
            .is_none());
        assert!(s.tracker(Some(&body), true, None).snapshot().is_none());
    }
    #[test]
    fn samples_full_stream_without_counting_local_time_or_placeholders() {
        let mut t = tracker();
        t.advance(Duration::from_secs(2));
        t.observe(&json!({"type":"response.output_text.delta","delta":"a"}));
        // Local processing does not call advance. A heartbeat does not reset progress.
        t.advance(Duration::from_secs(15));
        t.observe(&json!({"type":"response.transport_progress.delta","delta":" "}));
        t.advance(Duration::from_secs(15));
        t.observe(&json!({"type":"response.output_text.delta","delta":"b"}));
        t.observe(&json!({"type":"response.completed","response":{"status":"completed"}}));
        let s = t.snapshot().unwrap();
        assert!(s.complete);
        assert_eq!(s.first_ready_ms, Some(2000));
        assert_eq!(s.max_gap_ms, 30000);
        assert_eq!(s.long_gaps, 1);
        assert_eq!(s.wait_ms, 32000);
    }
    #[test]
    fn reasoning_and_interleaved_tools_are_progress() {
        let mut t = tracker();
        for _ in 0..20 {
            t.advance(Duration::from_secs(5));
            t.observe(&json!({"type":"response.reasoning_text.delta","delta":"thinking"}));
        }
        assert_eq!(t.snapshot().unwrap().max_gap_ms, 5000);
        assert!(t.snapshot().unwrap().first_ready_ms.is_none());
        for id in ["a", "b"] {
            t.observe(&json!({"type":"response.output_item.added","item":{"id":id,"type":"function_call"}}));
        }
        t.observe(&json!({"type":"response.output_item.done","item":{"id":"a"}}));
        t.advance(Duration::from_secs(30));
        t.observe(&json!({"type":"response.function_call_arguments.delta","delta":"{}"}));
        assert_eq!(t.snapshot().unwrap().tool_gap_ms, 30000);
    }
    #[test]
    fn failure_and_partial_are_not_healthy() {
        for terminal in [
            "response.failed",
            "response.incomplete",
            "response.cancelled",
        ] {
            let mut t = tracker();
            t.observe(&json!({"type":terminal}));
            assert!(!t.snapshot().unwrap().complete)
        }
        assert!(!tracker().snapshot().unwrap().complete)
    }
    #[test]
    fn disabled_and_oversized_are_neutral() {
        let mut t = Tracker::new(None);
        t.observe_sse(b"data: {}\n\n");
        assert!(t.snapshot().is_none());
        let mut t = tracker();
        t.observe_sse(&vec![b' '; (1 << 20) + 1]);
        assert!(t.snapshot().is_none())
    }
    #[test]
    fn multiline_sse_and_turn_reset() {
        let mut t = tracker();
        t.advance(Duration::from_secs(3));
        t.observe_sse(
            b"event: ignored\r\ndata: {\r\ndata: \"type\":\"response.completed\"}\r\n\r\n",
        );
        assert!(t.snapshot().unwrap().complete);
        let t = tracker();
        assert_eq!(t.snapshot().unwrap().wait_ms, 0);
        assert!(!t.snapshot().unwrap().complete)
    }
}

#[cfg(test)]
#[test]
fn session_partial_updates_apply_only_to_future_turns() {
    use serde_json::json;
    let mut session = Session::default();
    session.update(Some(&json!({"type":"session.update","session":{"reasoning":{"effort":"high"},"service_tier":"priority","tools":[{"type":"function"}]}})));
    let old = session.tracker(
        Some(&json!({"type":"response.create","model":"gpt-test"})),
        true,
        None,
    );
    session.update(Some(
        &json!({"type":"session.update","session":{"model":"other"}}),
    ));
    let next = session
        .tracker(
            Some(&json!({"type":"response.create","model":"gpt-test"})),
            true,
            None,
        )
        .snapshot()
        .unwrap();
    assert_eq!(next.request.effort, "high");
    assert_eq!(next.request.service_tier, "priority");
    assert!(next.request.has_tools);
    session.update(Some(
        &json!({"type":"session.update","session":{"reasoning":{"effort":"low"}}}),
    ));
    assert_eq!(old.snapshot().unwrap().request.effort, "high");
    assert_eq!(
        session
            .tracker(Some(&json!({"model":"gpt-test"})), true, None)
            .snapshot()
            .unwrap()
            .request
            .effort,
        "low"
    );
}
