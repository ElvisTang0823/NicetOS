# 導入函式庫
import json
import re
from urllib.parse import urlparse

from hash import get_stable_hash

HASH_CAPACITY = 142867


# ==========================
# 拆解網址：分成網域、路徑、參數等，只留下最乾淨的網址
def extract_domain(input_link: str) -> str:
    if '://' not in input_link:
        input_link = 'http://' + input_link
    netloc = urlparse(input_link).netloc
    domain = netloc.split(':')[0].rstrip('.')
    if domain.startswith('www.'):
        domain = domain[4:]
    return domain


# ===========================
# 讀取白名單 / 黑名單
with open('data/whitelist.json', 'r', encoding='utf-8') as file:
    whitelist = json.load(file)
with open('data/blacklist.json', 'r', encoding='utf-8') as file:
    blacklist = json.load(file)


# ==========================
# 邏輯判斷的function：先把 domain 轉成 hash，再與黑白名單比對
# 回傳 True => 白名單
# 回傳 False => 黑名單
# 回傳 0 => 未命中，交由 Go 或 SLM 判斷
# ==========================
def get_hash_key(input_link: str) -> str:
    domain = extract_domain(input_link)
    if not domain:
        return ""
    return str(get_stable_hash(domain, HASH_CAPACITY))


def check_url(input_link):
    hash_key = get_hash_key(input_link)
    if not hash_key:
        return 0

    # 黑名單優先，避免同時存在於兩邊時被白名單覆蓋
    if hash_key in blacklist:
        return False
    if hash_key in whitelist:
        return True
    return 0


# ==========================
# 直接開檔案測試
if __name__ == '__main__':
    result = check_url(str(input("請輸入網址: ")))  # 到時候改由 Go 傳入資料
    print(result)
