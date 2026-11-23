# One setting of a job in holdfast_jobs as the flags of holdfast job set (the kabsdk.holdfast.agent role).
from __future__ import annotations

from collections.abc import Mapping, Sequence

from ansible.errors import AnsibleFilterError

DOCUMENTATION = r"""
name: job_flag
short_description: The holdfast job set flags of one setting of a job
description:
  - Turns one key and value of a job in holdfast_jobs into the flags of holdfast job set.
  - An underscore in the key is a dash. A list repeats the flag, true gives the flag alone, false gives nothing.
options:
  _input:
    description: One item of dict2items, with key and value.
    type: dict
    required: true
"""

EXAMPLES = r"""
flags: "{{ {'path': ['/etc', '/home'], 'daily': '02:00'} | dict2items | map('kabsdk.holdfast.job_flag') | flatten }}"
# ['--path', '/etc', '--path', '/home', '--daily', '02:00']
"""

RETURN = r"""
_value:
  description: The flags, such as ["--path", "/etc"].
  type: list
  elements: str
"""


def job_flag(item):
    key, value = item["key"], item["value"]
    flag = "--" + str(key).replace("_", "-")
    if isinstance(value, bool):
        return [flag] if value else []
    if isinstance(value, Mapping) or value is None:
        raise AnsibleFilterError(f"holdfast_jobs: {key}: give a string, a number, a list of them, or true or false")
    if isinstance(value, Sequence) and not isinstance(value, str):
        out = []
        for v in value:
            if isinstance(v, (Mapping, bool)) or (isinstance(v, Sequence) and not isinstance(v, str)):
                raise AnsibleFilterError(f"holdfast_jobs: {key}: each item of the list is a string or a number")
            out += [flag, str(v)]
        return out
    return [flag, str(value)]


class FilterModule:
    def filters(self):
        return {"job_flag": job_flag}
