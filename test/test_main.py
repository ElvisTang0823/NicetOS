from main import check_url

def test_whitelist_only_coolenglish():
    assert check_url("https://www.coolenglish.edu.tw/event/download.php") == True

def test_whitelist_only_nnkieh():
    assert check_url("https://hs.nnkieh.tn.edu.tw/modules/tadnews/page.php?nsn=21#PageTab1") == True

def test_blacklist_only_pornhub():
    assert check_url("https://pornhub.com/xxx") == False

def test_blacklist_only_threads():
    assert check_url("https://threads.com/cba") == False

def test_blacklist_only_threads():
    assert check_url("https://www.instagram.com/") == False

def test_unknown_returns_zero():
    assert check_url("https://github.com/ElvisTang0823/NicetOS/tree/main/test") == 0