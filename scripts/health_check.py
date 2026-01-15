#!/usr/bin/env python3
import subprocess
import requests
import time
import argparse
from datetime import datetime

def checkPortAlive(request_timeout: float = 5.0) -> bool:
    try:
        with open("/home/aiges/.status", "r") as file:
            content = file.read().strip()
        with open("/home/aiges/sglangport", "r") as file:
            sglangPort = file.read().strip()

        if ":" in content:
            ip, port = content.split(":")
            print("IP:", ip)
            print("Port:", port)
        else:
            print("Invalid content format in the file.")
            exit(-2)

        print(f"check addr {ip}:{port}\n")
        rpc_checker_cmd = f"nc -zv {ip} {port}"
        ret = subprocess.call(rpc_checker_cmd, shell=True)
        if ret != 0:
            exit(ret)
        print(f"check sglang addr {ip}:{sglangPort}\n")
        url = f"http://{ip}:{sglangPort}/health"

        formatted = datetime.now().strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
        print("start_time:", formatted)  
        response = requests.get(url, timeout=request_timeout)
        print(response)
        response.raise_for_status()
        formatted = datetime.now().strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
        print("end_time:", formatted)
    except Exception as e:
        print(str(e))
        exit(-1)

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description="Health check with configurable timeout")
    parser.add_argument("--timeout", type=float, default=5.0, help="HTTP request timeout in seconds (default: 5.0)")
    args = parser.parse_args()
    print(f"request_timeout: {args.timeout}")
    checkPortAlive(request_timeout=args.timeout)