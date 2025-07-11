#!/usr/bin/env python3
import subprocess

def checkPortAlive() -> bool:
    try:
        with open("/home/aiges/.status", "r") as file:
            content = file.read().strip()

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
    except Exception as e:
        print(str(e))
        exit(-1)

if __name__ == '__main__':
    checkPortAlive()