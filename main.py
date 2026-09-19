# 導入函式庫
import json
import os
import re
import time
from urllib.parse import urlparse

try:
    from hash import get_stable_hash
except ModuleNotFoundError:
    from python.hash import get_stable_hash

try:
    import requests
except ImportError:  # pragma: no cover - only used for optional AI fallback
    requests = None

try:
    from bs4 import BeautifulSoup
except ImportError:  # pragma: no cover - optional dependency for AI fallback
    BeautifulSoup = None

HASH_CAPACITY = 142867
OLLAMA_API = "http://localhost:11434/api/chat"
MODEL_NAME = "gemma4-child-safety"
CACHE_TTL_SECONDS = 300
URL_CACHE = {}
RETRY_QUEUE = set()


# ==========================
# 拆解網址：分成網域、路徑、參數等，只留下最乾淨的網址
def extract_domain(input_link: str) -> str:
    if '://' not in input_link:
        input_link = 'http://' + input_link
    hostname = urlparse(input_link).hostname
    if hostname is None:
        return ''
    hostname = hostname.rstrip('.')
    try:
        hostname = hostname.encode('idna').decode('ascii')
    except UnicodeError:
        return ''
    if hostname.startswith('www.'):
        hostname = hostname[4:]
    return hostname


def load_hash_map(filepath: str) -> dict[str, list[str]]:
    if not os.path.exists(filepath):
        return {}

    with open(filepath, 'r', encoding='utf-8') as file:
        data = json.load(file)

    normalized = {}
    for key, value in data.items():
        normalized[str(key)] = value if isinstance(value, list) else [value]
    return normalized


def domain_in_bucket(bucket: list[str], domain: str) -> bool:
    return domain in bucket


# ===========================
# 讀取白名單 / 黑名單
# 以 main.py 自身所在目錄為資料根，不再依賴執行時的工作目錄
BASE_DIR = os.path.dirname(os.path.abspath(__file__))
DATA_DIR = os.path.join(BASE_DIR, 'data')

whitelist = load_hash_map(os.path.join(DATA_DIR, 'whitelist.json'))
blacklist = load_hash_map(os.path.join(DATA_DIR, 'blacklist.json'))


# ==========================
# 邏輯判斷的function：先把 domain 轉成 hash，再與黑白名單比對
# 回傳 True => 白名單
# 回傳 False => 黑名單
# 回傳 0 => 未命中，交由 Go 或 SLM 判斷
# ==========================
def get_hash_key(domain: str) -> str:
    if not domain:
        return ""
    return str(get_stable_hash(domain, HASH_CAPACITY))


def build_cache_key(url: str) -> str:
    parsed = urlparse(url if '://' in url else 'http://' + url)
    scheme = (parsed.scheme or 'http').lower()
    netloc = (parsed.netloc or parsed.hostname or '').lower()
    port = parsed.port if parsed.port is not None else ''
    path = parsed.path or '/'
    query = parsed.query or ''
    return json.dumps({
        'scheme': scheme,
        'netloc': netloc,
        'port': port,
        'path': path,
        'query': query,
    }, separators=(',', ':'), ensure_ascii=False)


def get_cached_decision(url: str):
    key = build_cache_key(url)
    cached = URL_CACHE.get(key)
    if not cached:
        return None
    if time.monotonic() >= cached['expires_at']:
        URL_CACHE.pop(key, None)
        return None
    return cached['decision']


def set_cached_decision(url: str, decision):
    key = build_cache_key(url)
    URL_CACHE[key] = {
        'decision': decision,
        'expires_at': time.monotonic() + CACHE_TTL_SECONDS,
    }


def queue_retry(url: str):
    RETRY_QUEUE.add(build_cache_key(url))


def fetch_webpage_info(url: str):
    """自動抓取網頁標題、Meta 說明與內文段落"""
    if requests is None or BeautifulSoup is None:
        return None, None

    headers = {
        "User-Agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
            "AppleWebKit/537.36 (KHTML, like Gecko) "
            "Chrome/120.0.0.0 Safari/537.36"
        )
    }

    try:
        response = requests.get(url, headers=headers, timeout=15)
        response.encoding = response.apparent_encoding or "utf-8"
        response.raise_for_status()

        soup = BeautifulSoup(response.text, "html.parser")
        title = soup.title.string.strip() if soup.title and soup.title.string else "無標題"

        description_tag = soup.find("meta", attrs={"name": re.compile(r"description", re.I)}) or \
            soup.find("meta", attrs={"property": "og:description"})
        description = description_tag["content"].strip() if description_tag and description_tag.get("content") else ""

        paragraphs = [p.get_text().strip() for p in soup.find_all("p") if p.get_text().strip()]
        page_body = " ".join(paragraphs[:3])
        full_content = f"{description} {page_body}".strip()
        if not full_content:
            full_content = "無網頁內容說明"
        return title, full_content
    except Exception:
        return None, None


def check_url_safety(url: str):
    """自動爬取網頁並呼叫 Ollama 判斷兒少安全性"""
    if requests is None:
        return None

    title, content = fetch_webpage_info(url)
    if not title:
        return None

    user_prompt = f"""[網頁資訊]
網址: {url}
標題: {title}
內容: {content[:400]}

請嚴格僅輸出純 JSON，格式如下：
{{
  "decision": "ALLOW" 或 "BLOCK",
  "category": "分類名稱",
  "confidence": 1.0,
  "reason": "簡短說明 (50字以內)"
}}"""

    payload = {
        "model": MODEL_NAME,
        "messages": [
            {
                "role": "system",
                "content": "Respond strictly with pure raw JSON object only. Do not wrap in ```json markdown code block.",
            },
            {"role": "user", "content": user_prompt},
        ],
        "stream": False,
        "options": {
            "temperature": 0.0,
        },
        "keep_alive": "1h",
    }

    try:
        response = requests.post(OLLAMA_API, json=payload, timeout=30)
        response.raise_for_status()
        result = response.json()

        raw_output = result.get("message", {}).get("content", "")
        clean_str = re.sub(r"```json|```", "", raw_output).strip()
        json_match = re.search(r"\{[\s\S]*\}", clean_str)
        if json_match:
            clean_str = json_match.group(0)
        return json.loads(clean_str)
    except Exception:
        return None


def check_url(input_link):
    if not input_link:
        return 0

    cached = get_cached_decision(input_link)
    if cached is not None:
        return cached

    domain = extract_domain(input_link)
    if not domain:
        queue_retry(input_link)
        set_cached_decision(input_link, 0)
        return 0

    hash_key = get_hash_key(domain)
    if not hash_key:
        queue_retry(input_link)
        set_cached_decision(input_link, 0)
        return 0

    if hash_key in blacklist and domain_in_bucket(blacklist[hash_key], domain):
        set_cached_decision(input_link, False)
        return False

    if hash_key in whitelist and domain_in_bucket(whitelist[hash_key], domain):
        set_cached_decision(input_link, True)
        return True

    try:
        decision = check_url_safety(input_link)
        if decision is None:
            queue_retry(input_link)
            set_cached_decision(input_link, 0)
            return 0
        parsed = str(decision.get("decision", "")).upper()
        if parsed == "BLOCK":
            set_cached_decision(input_link, False)
            return False
        if parsed == "ALLOW":
            set_cached_decision(input_link, True)
            return True
    except Exception:
        queue_retry(input_link)
        set_cached_decision(input_link, 0)
        return 0

    queue_retry(input_link)
    set_cached_decision(input_link, 0)
    return 0


# ==========================
# 直接開檔案測試
if __name__ == '__main__':
    target = ""
    if len(__import__('sys').argv) > 1:
        target = __import__('sys').argv[1]
    else:
        target = input("請輸入網址: ")
    print(check_url(str(target)))

