# 導入函式庫
import json
from urllib.parse import urlparse

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
# 讀取白名單
with open('data/whitelist.json', 'r', encoding='utf-8') as file:
    whitelist = json.load(file)
# 讀取黑名單
with open('data/blacklist.json', 'r', encoding='utf-8') as file:
    blacklist = json.load(file)

# ==========================
# 邏輯判斷的function：在白名單內為真/黑名單內為假/皆非則回傳0，接著讓Go判斷是否允許訪問
# 目前問題：若白名單與黑名單同時存在，會回傳True
# 目前問題2：時間複雜度O(n)，未來改成使用Trie樹或HashSet
def check_url(input_link):
    for url in whitelist:
        if url in input_link:
            return True
    for url in blacklist:
        if url in input_link:
            return False
    return 0 # 未來改成串接SLM判斷

# ==========================
# 直接開檔案測試
if __name__ == '__main__':
    result = check_url(str(input("請輸入網址: "))) # 到時候改由Go傳入資料
    print(result)
