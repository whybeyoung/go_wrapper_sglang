#!/usr/bin/env python3
import os
import time
import json
import requests
import logging
from argparse import ArgumentParser

import os
import re
import argparse
import logging
import asyncio

# 配置日志
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s',
    filename='/var/log/warmup.log',  # 指定日志文件路径
    filemode='a'
)
logger = logging.getLogger('model_warmup')

def validate_port(port_str):
    """验证端口号是否有效"""
    try:
        port = int(port_str.strip())
    except ValueError:
        raise argparse.ArgumentTypeError(f"无效的端口号: '{port_str}'")
    
    if 1 <= port <= 65535:
        return port
    else:
        raise argparse.ArgumentTypeError(f"端口号必须在1-65535之间: {port}")

def read_port_from_file(file_path):
    """从文件读取端口号并验证，最多重试1分钟"""
    max_retries = 12  # 12次重试，每次间隔5秒，总计60秒
    retry_delay = 5   # 每次重试间隔(秒)
    
    for attempt in range(max_retries):
        try:
            # 检查文件是否存在
            if not os.path.exists(file_path):
                raise FileNotFoundError(f"端口文件不存在: {file_path}")
            
            # 读取文件内容
            with open(file_path, 'r') as f:
                port_str = f.read().strip()
            
            # 验证内容有效性
            if not port_str:
                raise ValueError("端口文件为空")
            
            if not re.match(r'^\d+$', port_str):
                raise ValueError(f"端口文件包含非数字字符: '{port_str}'")
            
            port = int(port_str)
            
            if not (1 <= port <= 65535):
                raise ValueError(f"端口号超出范围: {port}")
            
            # 验证成功，返回端口号
            logger.info(f"成功从文件获取端口号: {port} (尝试 {attempt+1}/{max_retries})")
            return port
            
        except Exception as e:
            if attempt < max_retries - 1:  # 不是最后一次尝试
                logger.warning(f"读取端口文件失败(尝试 {attempt+1}/{max_retries}): {e}，{retry_delay}秒后重试")
                time.sleep(retry_delay)
            else:  # 最后一次尝试失败
                logger.error(f"读取端口文件失败，达到最大重试次数: {e}")
                raise argparse.ArgumentTypeError(f"读取端口文件失败: {e}")

def parse_args():
    parser = argparse.ArgumentParser(description='Model Warmup Script')
    
    # 添加端口文件参数
    parser.add_argument('--port-file', type=str, default='/home/aiges/sglangport',
                        help='包含端口号的文件路径')
    
    # 移除api-url参数，改为完全动态生成
    # parser.add_argument('--api-url', ...)  # 不再需要这个参数
    
    parser.add_argument('--warmup-data', type=str, default='/home/aiges/warmup-data.json',
                        help='预热请求数据JSON文件路径')
    parser.add_argument('--timeout', type=int, default=300,
                        help='等待服务就绪的超时时间(秒)')
    parser.add_argument('--interval', type=int, default=5,
                        help='检查服务状态的间隔时间(秒)')
    parser.add_argument('--max-retries', type=int, default=3,
                        help='每个预热请求的最大重试次数')
    
    args = parser.parse_args()
    
    # 从端口文件读取并构建api-url
    try:
        port = read_port_from_file(args.port_file)
        args.api_base = f"http://localhost:{port}"
        logger.info(f"从文件 {args.port_file} 获取端口号: {port}")
        logger.debug(f"构建API URL: {args.api_base}")
    except argparse.ArgumentTypeError as e:
        parser.error(str(e))
    
    return args

def wait_for_service_ready(api_url, timeout, interval):
    """等待服务就绪"""
    start_time = time.time()
    while time.time() - start_time < timeout:
        try:
            response = requests.get(api_url, timeout=5)
            if response.status_code == 200:
                logger.info("服务已就绪，开始预热...")
                return True
        except Exception as e:
            logger.warning(f"服务未就绪，等待中: {e}")
            time.sleep(interval)
    
    logger.error(f"服务在{timeout}秒内未就绪，预热失败")
    return False

def load_warmup_prompts(file_path):
    """加载预热请求数据"""
    if not os.path.exists(file_path):
        logger.error(f"预热数据文件不存在: {file_path}")
        return []
    
    try:
        with open(file_path, 'r') as f:
            return json.load(f)
    except Exception as e:
        logger.error(f"解析预热数据失败: {e}")
        return []

import aiohttp
import asyncio

async def send_warmup_request_async(session, api_url, payload, max_retries):
    """异步发送预热请求"""
    for attempt in range(max_retries):
        try:
            start_time = time.time()
            async with session.post(api_url, json=payload, timeout=30) as response:
                response.raise_for_status()
                end_time = time.time()
                logger.info(f"预热请求成功，耗时: {end_time - start_time:.2f}秒")
                return True
        except Exception as e:
            logger.warning(f"预热请求失败(尝试 {attempt+1}/{max_retries}): {e}")
            await asyncio.sleep(2)
    
    logger.error(f"预热请求达到最大重试次数，请求内容: {payload}")
    return False

async def main_async():
    args = parse_args()
    
    if not wait_for_service_ready(f"{args.api_base}/health", args.timeout, args.interval):
        exit(1)
    
    warmup_prompts = load_warmup_prompts(args.warmup_data)
    if not warmup_prompts:
        logger.warning("没有可用的预热请求数据，跳过预热")
        exit(0)
    total_count = len(warmup_prompts)
    async with aiohttp.ClientSession() as session:
        tasks = []
        for i, prompt_data in enumerate(warmup_prompts):
            logger.info(f"添加预热请求 {i+1}/{len(warmup_prompts)} 到队列")
            payload = {
                "model": "your-model-name",
                "messages": [{"role": "user", "content": prompt_data["prompt"]}],
                "max_tokens": prompt_data.get("max_tokens", 100),
                "temperature": prompt_data.get("temperature", 0.7)
            }
            tasks.append(send_warmup_request_async(session, f"{args.api_base}/v1/chat/completions", payload, args.max_retries))
        
        results = await asyncio.gather(*tasks)
        success_count = sum(results)
    
    
    logger.info(f"预热完成: {success_count}/{total_count} 请求成功")
    
    # 检查预热成功率
    success_rate = success_count / total_count if total_count > 0 else 0
    if success_rate < 0.8:
        logger.error(f"预热成功率低于80%，可能存在问题")
        exit(1)
    
    logger.info("预热脚本执行成功")
    exit(0)

if __name__ == "__main__":
    asyncio.run(main_async())