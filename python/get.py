import json
import os
import sys
from datetime import datetime

ROOT_DIR = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir))
SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
if SCRIPT_DIR not in sys.path:
    sys.path.insert(0, SCRIPT_DIR)

from requests_loader import load_requests

requests = load_requests(ROOT_DIR)

# NicetOS 執行中會把系統 Proxy 指向自己；下載名單時不經過它，
# 否則名單網址可能被自己封鎖，名單就永遠更新不了。
SESSION = requests.Session()
SESSION.trust_env = False

# 設定要下載的名單（可自行增減）
LISTS = {
    "blacklist": "https://raw.githubusercontent.com/ElvisTang0823/NicetOS/main/data/blacklist.json",
    "whitelist": "https://raw.githubusercontent.com/ElvisTang0823/NicetOS/main/data/whitelist.json",
}

SAVE_DIR = os.environ.get("NICETOS_DATA_DIR", os.path.join(ROOT_DIR, "data"))


def download_list(name, url, save_dir):
    os.makedirs(save_dir, exist_ok=True)
    save_path = os.path.join(save_dir, f"{name}.json")

    try:
        response = SESSION.get(url, timeout=10)
        response.raise_for_status()
        new_content = response.text

        # 先確認下載內容是正確的 JSON 名單；壞掉的檔案會讓 NicetOS 下次無法啟動。
        try:
            parsed = json.loads(new_content)
        except ValueError as exc:
            print(f"[{datetime.now()}] {name} 下載內容不是有效的 JSON，保留舊名單：{exc}")
            return False
        if not isinstance(parsed, dict) or not all(
            isinstance(value, (list, str)) for value in parsed.values()
        ):
            print(f"[{datetime.now()}] {name} 下載內容格式不正確，保留舊名單")
            return False

        # 檢查是否有變更（避免重複覆寫沒必要的更新）
        old_content = ""
        if os.path.exists(save_path):
            with open(save_path, "r", encoding="utf-8") as f:
                old_content = f.read()

        if new_content != old_content:
            # 先寫到暫存檔再換名，寫到一半中斷也不會弄壞原本的名單。
            temporary_path = save_path + ".new"
            with open(temporary_path, "w", encoding="utf-8") as f:
                f.write(new_content)
            os.replace(temporary_path, save_path)
            print(f"[{datetime.now()}] {name} 已更新")
        else:
            print(f"[{datetime.now()}] {name} 無變更")

        return True
    except requests.exceptions.RequestException as e:
        print(f"[{datetime.now()}] {name} 下載失敗：{e}")
        return False


def update_all_lists():
    for name, url in LISTS.items():
        download_list(name, url, SAVE_DIR)


if __name__ == "__main__":
    update_all_lists()
