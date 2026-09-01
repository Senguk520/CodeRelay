use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Account {
    pub id: String,
    pub email: String,
    pub region: String,
    pub plan: String,
    pub status: String,
    pub quota: f64,
    pub quota_total: f64,
    pub last_used: Option<i64>,
    pub failures: u32,
    pub tags: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub uid: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub enterprise_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub domain: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_checkin: Option<i64>,
    #[serde(default)]
    pub checkin_streak: u32,
    // 兼容旧版 state.json。新的保存流程会在落盘前清除此字段，凭据仅写入 credentials.json。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub access_token: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refresh_token: Option<String>,
}

impl Default for Account {
    fn default() -> Self {
        Self {
            id: String::new(),
            email: String::new(),
            region: "cn".to_string(),
            plan: "FREE".to_string(),
            status: "needs_auth".to_string(),
            quota: 0.0,
            quota_total: 0.0,
            last_used: None,
            failures: 0,
            tags: Vec::new(),
            uid: None,
            enterprise_id: None,
            domain: None,
            last_checkin: None,
            checkin_streak: 0,
            access_token: None,
            refresh_token: None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AccountCredential {
    pub account_id: String,
    pub access_token: String,
    pub refresh_token: Option<String>,
}

impl Default for AccountCredential {
    fn default() -> Self {
        Self {
            account_id: String::new(),
            access_token: String::new(),
            refresh_token: None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ApiKey {
    pub id: String,
    pub name: String,
    pub key: String,
    pub enabled: bool,
    pub account_ids: Option<Vec<String>>,
    pub models: Vec<String>,
    pub created_at: i64,
    pub last_used: Option<i64>,
}

impl Default for ApiKey {
    fn default() -> Self {
        Self {
            id: String::new(),
            name: String::new(),
            key: String::new(),
            enabled: true,
            account_ids: None,
            models: Vec::new(),
            created_at: 0,
            last_used: None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ServiceConfig {
    pub enabled: bool,
    pub port: u16,
    pub bind_host: String,
    pub scope: String,
    pub request_timeout_ms: u64,
    pub max_retries: u8,
    pub routing_strategy: String,
    pub session_affinity: bool,
    pub vision_tool_enabled: bool,
    pub image_generation_mode: String,
    pub debug_logs: bool,
}

impl Default for ServiceConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            port: 11435,
            bind_host: "127.0.0.1".to_string(),
            scope: "localhost".to_string(),
            request_timeout_ms: 120_000,
            max_retries: 2,
            routing_strategy: "auto".to_string(),
            session_affinity: true,
            vision_tool_enabled: true,
            image_generation_mode: "enabled".to_string(),
            debug_logs: false,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct HourStats {
    pub label: String,
    pub hit: u64,
    pub miss: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct UsageStats {
    pub request_count: u64,
    pub total_tokens: u64,
    pub cache_hit_tokens: u64,
    pub credit: f64,
    pub average_latency_ms: u64,
    pub success_count: u64,
    pub failure_count: u64,
    pub by_hour: Vec<HourStats>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct RequestLog {
    pub request_id: String,
    pub timestamp: i64,
    pub method: String,
    pub path: String,
    pub model: String,
    pub account_id: String,
    pub api_key_id: String,
    pub status: u16,
    pub success: bool,
    pub latency_ms: u64,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub credit: f64,
    pub cache_hit: bool,
    pub error: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AppState {
    pub config: ServiceConfig,
    pub accounts: Vec<Account>,
    pub keys: Vec<ApiKey>,
    pub logs: Vec<RequestLog>,
    pub stats: UsageStats,
    pub running: bool,
    pub actual_port: Option<u16>,
    pub last_error: Option<String>,
}

impl Default for AppState {
    fn default() -> Self {
        Self {
            config: ServiceConfig::default(),
            accounts: Vec::new(),
            keys: Vec::new(),
            logs: Vec::new(),
            stats: UsageStats::default(),
            running: false,
            actual_port: None,
            last_error: None,
        }
    }
}

impl AppState {
    pub fn rebuild_stats(&mut self) {
        let mut stats = UsageStats {
            by_hour: (0..8)
                .map(|bucket| HourStats {
                    label: format!("{:02}", bucket * 3),
                    hit: 0,
                    miss: 0,
                })
                .collect(),
            ..UsageStats::default()
        };
        let mut total_latency = 0_u64;
        for log in &self.logs {
            stats.request_count = stats.request_count.saturating_add(1);
            stats.total_tokens = stats
                .total_tokens
                .saturating_add(log.input_tokens.saturating_add(log.output_tokens));
            if log.cache_hit {
                stats.cache_hit_tokens = stats.cache_hit_tokens.saturating_add(log.input_tokens);
            }
            stats.credit += log.credit;
            total_latency = total_latency.saturating_add(log.latency_ms);
            if log.success {
                stats.success_count = stats.success_count.saturating_add(1);
            } else {
                stats.failure_count = stats.failure_count.saturating_add(1);
            }
            let hour = ((log.timestamp.div_euclid(3_600_000)).rem_euclid(24)) as usize;
            let bucket = (hour / 3).min(7);
            if log.cache_hit {
                stats.by_hour[bucket].hit = stats.by_hour[bucket].hit.saturating_add(1);
            } else {
                stats.by_hour[bucket].miss = stats.by_hour[bucket].miss.saturating_add(1);
            }
        }
        if stats.request_count > 0 {
            stats.average_latency_ms = total_latency / stats.request_count;
        }
        self.stats = stats;
    }

    pub fn sanitize_for_persistence(&mut self) {
        self.running = false;
        self.actual_port = None;
        for account in &mut self.accounts {
            account.access_token = None;
            account.refresh_token = None;
        }
    }
}
