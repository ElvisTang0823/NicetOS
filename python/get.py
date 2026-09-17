import requests
from datetime import datetime
import os

# 設定要下載的名單（可自行增減）
LISTS = {
    "blacklist": "https://raw.githubusercontent.com/ElvisTang0823/NicetOS/main/data/blacklist.json",
    "whitelist": "https://raw.githubusercontent.com/ElvisTang0823/NicetOS/main/data/whitelist.json",
}

SAVE_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "data")


def download_list(name, url, save_dir):
    os.makedirs(save_dir, exist_ok=True)
    save_path = os.path.join(save_dir, f"{name}.json")

    try:
        response = requests.get(url, timeout=10)
        response.raise_for_status()
        new_content = response.text

        # 檢查是否有變更（避免重複覆寫沒必要的更新）
        old_content = ""
        if os.path.exists(save_path):
            with open(save_path, "r", encoding="utf-8") as f:
                old_content = f.read()

        if new_content != old_content:
            with open(save_path, "w", encoding="utf-8") as f:
                f.write(new_content)
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
