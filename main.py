# 導入函式庫
import json
import os
from urllib.parse import urlparse

try:
    from hash import get_stable_hash
except ModuleNotFoundError:
    from python.hash import get_stable_hash

HASH_CAPACITY = 142867


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
whitelist = load_hash_map('data/whitelist.json')
blacklist = load_hash_map('data/blacklist.json')


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


def check_url(input_link):
    domain = extract_domain(input_link)
    if not domain:
        return 0

    hash_key = get_hash_key(domain)
    if not hash_key:
        return 0

    if hash_key in blacklist and domain_in_bucket(blacklist[hash_key], domain):
        return False

    if hash_key in whitelist and domain_in_bucket(whitelist[hash_key], domain):
        return True

    return 0

# ==========================
# 直接開檔案測試
if __name__ == '__main__':
    result = check_url(str(input("請輸入網址: ")))  # 到時候改由 Go 傳入資料
    print(result)

