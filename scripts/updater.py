import requests
import re
import sys
import os
from concurrent.futures import ThreadPoolExecutor

# Cursor 自动指纹更新工具 (Python版)
# 运行环境: python 3.x
# 依赖库: pip install requests

# 如果需要手动指定代理，请取消下面注释并填入你的代理地址
PROXIES = {"http": "http://127.0.0.1:10808", "https": "http://127.0.0.1:10808"}
#PROXIES = None 

def get_latest_script_info():
    # 自动获取环境变量中的代理配置
    request_proxies = PROXIES
    if not request_proxies:
        env_http = os.environ.get('HTTP_PROXY') or os.environ.get('http_proxy')
        env_https = os.environ.get('HTTPS_PROXY') or os.environ.get('https_proxy')
        if env_http or env_https:
            request_proxies = {
                "http": env_http,
                "https": env_https
            }

    targets = [
        "https://www.cursor.com/",
        "https://www.cursor.com/login",
        "https://www.cursor.com/settings"
    ]
    
    headers = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
    }

    print("正在扫描 Cursor 资源页面...")
    if request_proxies:
        print(f"检测到代理配置: {request_proxies}")
    found_urls = set()
    
    for url in targets:
        try:
            print(f"扫描: {url} ...")
            response = requests.get(url, headers=headers, timeout=20, proxies=request_proxies)
            if response.status_code == 200:
                # 匹配 assets 中的关键 JS 文件
                matches = re.findall(r'/assets/(?:chat|index|Login)-[a-f0-9]+\.js', response.text)
                for m in matches:
                    found_urls.add("https://www.cursor.com" + m)
        except Exception as e:
            print(f"读取页面 {url} 失败: {e}")

    if not found_urls:
        print("\n[!] 未能通过静态扫描发现脚本。Cursor 可能加强了混淆。")
        print("[*] 推荐手动抓取路径: https://www.cursor.com/assets/index-75051939.js")
        return

    print(f"\n找到 {len(found_urls)} 个候选脚本:")
    
    version_pattern = re.compile(r'["\'](\d+\.\d+\.\d+)["\']')
    
    for script_url in found_urls:
        print(f"\n--- 脚本: {script_url} ---")
        try:
            # 只读取脚本的前 50KB 内容，防止内存溢出且提高效率
            js_resp = requests.get(script_url, headers=headers, stream=True, timeout=10, proxies=request_proxies)
            chunk = js_resp.raw.read(1024 * 50).decode('utf-8', errors='ignore')
            
            # 查找版本号特征
            # 寻找类似 "0.45.14" 这种格式
            potential_versions = version_pattern.findall(chunk)
            # 过滤掉一些明显的无关字符串，保留符合 x.y.z 格式的
            valid_versions = [v for v in potential_versions if v.startswith('0.')]
            
            if valid_versions:
                # 通常版本号会重复出现，取频率最高或最后一个
                latest_v = valid_versions[-1]
                print(f"[✓] 探测到可能内核版本号: {latest_v}")
            else:
                print("[?] 未能从代码片段中提取到版本号。")
                
        except Exception as e:
            print(f"[!] 无法进一步分析该脚本内容: {e}")

    print("\n请根据以上结果更新您的 .env (SCRIPT_URL) 和 headers.go (version)。")

if __name__ == "__main__":
    try:
        get_latest_script_info()
    except KeyboardInterrupt:
        print("\n用户中止。")
        sys.exit(0)
