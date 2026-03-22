#!/usr/bin/env python3

import json
import sys
from typing import Any, Dict, List, Set


RECENT_MESSAGE_COUNT = 6
MIN_MESSAGES_TO_COMPACT = RECENT_MESSAGE_COUNT + 2


def parse_json_payload(value: Any) -> List[Dict[str, Any]]:
    candidates = []  # type: List[str]
    if isinstance(value, str):
        candidates.append(value)
    elif isinstance(value, dict):
        candidates.append(json.dumps(value, ensure_ascii=True))

    parsed = []  # type: List[Dict[str, Any]]
    for candidate in candidates:
        if '"test_id"' not in candidate and '"finding_id"' not in candidate:
            continue
        try:
            data = json.loads(candidate)
        except (json.JSONDecodeError, TypeError):
            continue
        if not isinstance(data, dict):
            continue
        if ("test_id" in data and "crash_triggered" in data) or "finding_id" in data:
            parsed.append(data)
    return parsed


def format_test_result(payload: Dict[str, Any]) -> str:
    return "Test {test_id}: {description} | crash={crash} | mutations={mutations} | packets={packets} | new_states={new_states}".format(
        test_id=payload.get("test_id"),
        description=str(payload.get("fuzz_description", payload.get("description", "")))[:80],
        crash=payload.get("crash_triggered", False),
        mutations=payload.get("mutation_count", "?"),
        packets=payload.get("total_packets", "?"),
        new_states=len(payload.get("new_states", []) or []),
    )


def format_finding(payload: Dict[str, Any]) -> str:
    return "Finding {finding_id}: {description}".format(
        finding_id=payload.get("finding_id"),
        description=str(payload.get("description", ""))[:150],
    )


def collect_tool_results(content: Any) -> List[str]:
    return [
        format_test_result(payload)
        for payload in parse_json_payload(content)
        if "test_id" in payload and "crash_triggered" in payload
    ]


def collect_tool_findings(content: Any) -> List[str]:
    return [format_finding(payload) for payload in parse_json_payload(content) if "finding_id" in payload]


def collect_legacy_results(content: Any) -> List[str]:
    if not isinstance(content, list):
        return []

    results = []  # type: List[str]
    for block in content:
        if not isinstance(block, dict):
            continue
        results.extend(collect_tool_results(block.get("content", "")))
    return results


def collect_legacy_findings(content: Any) -> List[str]:
    if not isinstance(content, list):
        return []

    findings = []  # type: List[str]
    for block in content:
        if not isinstance(block, dict):
            continue
        findings.extend(collect_tool_findings(block.get("content", "")))
    return findings


def collect_reasoning(message: Dict[str, Any]) -> List[str]:
    content = message.get("content", "")
    if not isinstance(content, str) or not content:
        return []

    keywords = (
        "hypothesis",
        "because",
        "suggest",
        "interest",
        "approach",
        "insight",
        "crash",
        "learn",
        "strateg",
        "conclusion",
        "important",
        "discover",
    )

    reasoning = []  # type: List[str]
    for sentence in content.split(". "):
        lower = sentence.lower()
        if any(keyword in lower for keyword in keywords):
            cleaned = sentence.strip()[:200]
            if len(cleaned) > 30:
                reasoning.append(cleaned)
    return reasoning


def dedupe_recent_reasoning(reasoning: List[str]) -> List[str]:
    seen = set()  # type: Set[str]
    unique = []  # type: List[str]
    for item in reversed(reasoning):
        key = item[:50].lower()
        if key in seen:
            continue
        seen.add(key)
        unique.append(item)
    return list(reversed(unique[:10]))


def build_summary(old_messages: List[Dict[str, Any]]) -> str:
    test_results = []  # type: List[str]
    findings = []  # type: List[str]
    reasoning = []  # type: List[str]

    for message in old_messages:
        content = message.get("content", "")
        role = message.get("role")

        if role == "tool":
            test_results.extend(collect_tool_results(content))
            findings.extend(collect_tool_findings(content))

        test_results.extend(collect_legacy_results(content))
        findings.extend(collect_legacy_findings(content))

        if role == "assistant":
            reasoning.extend(collect_reasoning(message))

    parts = ["## Conversation Summary (older turns condensed)"]

    if test_results:
        parts.append("\n### Test Results")
        for item in test_results:
            parts.append(f"- {item}")

    if findings:
        parts.append("\n### Key Findings Recorded")
        for item in findings:
            parts.append(f"- {item}")

    deduped_reasoning = dedupe_recent_reasoning(reasoning)
    if deduped_reasoning:
        parts.append("\n### Agent Reasoning Highlights")
        for item in deduped_reasoning:
            parts.append(f"- {item}")

    parts.append(
        "\n(Full findings and test history are preserved in the knowledge base. Use get_test_history and query_findings to access them.)"
    )
    return "\n".join(parts)


def compact_messages(messages: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    if len(messages) <= MIN_MESSAGES_TO_COMPACT:
        return messages

    old_messages = messages[:-RECENT_MESSAGE_COUNT]
    recent_messages = messages[-RECENT_MESSAGE_COUNT:]
    if not old_messages:
        return messages

    summary = build_summary(old_messages)
    return [{"role": "user", "content": summary}] + recent_messages


def main() -> int:
    try:
        messages = json.load(sys.stdin)
    except json.JSONDecodeError as exc:
        print(f"Failed to parse stdin JSON: {exc}", file=sys.stderr)
        return 1

    if not isinstance(messages, list):
        print("Expected a JSON array of messages on stdin", file=sys.stderr)
        return 1

    compacted = compact_messages(messages)
    json.dump(compacted, sys.stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())