import json
import hashlib
import os

def get_stable_hash(text: str, capacity: int) -> int:
    """使用 MD5 計算固定不變的雜湊索引"""
    hex_hash = hashlib.md5(text.encode('utf-8')).hexdigest()
    return int(hex_hash, 16) % capacity

def add_to_hash_json(input_text: str, category: str, capacity: int = 142867, data_dir: str = "./data"):
    """
    category: 'whitelist' 或 'blacklist'
    """
    filename = os.path.join(data_dir, f"{category}.json")
    
    # 0. 確保資料夾路徑存在
    os.makedirs(data_dir, exist_ok=True)
    
    # 1. 讀取現有的 JSON 檔案資料
    hash_map = {}
    if os.path.exists(filename):
        try:
            with open(filename, "r", encoding="utf-8") as f:
                existing_data = json.load(f)
                # 將字串 Key 轉回 Integer，並將 Value 統一轉成 List
                for k, v in existing_data.items():
                    hash_map[int(k)] = v if isinstance(v, list) else [v]
        except json.JSONDecodeError:
            hash_map = {}

    # 2. 解析輸入的字串並計算 Hash
    words = set(input_text.split())
    for word in words:
        index = get_stable_hash(word, capacity)
        
        if index not in hash_map:
            hash_map[index] = []
            
        # 防重複機制
        if word not in hash_map[index]:
            hash_map[index].append(word)

    # 3. 依 Hash 數字大小進行 Key 排序
    sorted_dict = {}
    for k in sorted(hash_map.keys()):
        words_in_bucket = hash_map[k]
        # 碰撞時維持 List，單一元素解包為字串
        sorted_dict[str(k)] = words_in_bucket[0] if len(words_in_bucket) == 1 else words_in_bucket

    # 4. 寫回 .json 檔案
    with open(filename, "w", encoding="utf-8") as f:
        json.dump(sorted_dict, f, ensure_ascii=False, indent=4)

    return filename, sorted_dict

def main():
    print("=== 域名/字串 Hash Table 字典維護工具 ===")
    
    # 1. 輸入網址或字串
    user_input = input("請輸入網址或字串 (多個可用空格隔開): ").strip()
    if not user_input:
        print("未輸入任何內容，程式結束。")
        return

    # 2. 選擇目標清單
    print("\n請選擇要存入的類別：")
    print(" [1] Whitelist (白名單)")
    print(" [2] Blacklist (黑名單)")
    choice = input("請輸入選項 (1 或 2): ").strip()

    category_map = {"1": "whitelist", "2": "blacklist"}
    category = category_map.get(choice)

    if not category:
        print("選項錯誤！必須選擇 1 或 2，程式結束。")
        return

    # 3. 執行更新與寫入
    filepath, updated_dict = add_to_hash_json(user_input, category)

    print("\n" + "=" * 40)
    print(f"成功新增至 [{category}]！")
    print(f"檔案位置: {filepath}")
    print("最新的字典內容：")
    print(json.dumps(updated_dict, ensure_ascii=False, indent=4))
    print("=" * 40)

if __name__ == "__main__":
    main()