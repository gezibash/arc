#!/usr/bin/env python3
"""Create and tear down the isolated EC2 traversal-verification lab.

The program uses only the installed AWS CLI and Python's standard library. It
records every created ID in a local state file so teardown has an exact target.
It intentionally does not create IAM roles, copy SSH private keys, or deploy
ARC code. See docs/transport/VERIFICATION.md for the experiment itself.
"""

from __future__ import annotations

import argparse
import datetime as dt
import ipaddress
import json
import os
import re
import secrets
import subprocess
import sys
from pathlib import Path
from typing import Any


PROJECT_TAG = "arc-traversal-lab"
DEFAULT_REGION = "eu-west-1"
DEFAULT_AMI_PARAMETER = (
    "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
)
STATE_VERSION = 1


class LabError(RuntimeError):
    """A user-actionable lab setup error."""


def aws(profile: str, region: str, *args: str) -> Any:
    command = ["aws", "--no-cli-pager", "--profile", profile, "--region", region, *args]
    result = subprocess.run(command, check=False, capture_output=True, text=True)
    if result.returncode:
        detail = result.stderr.strip() or result.stdout.strip() or "AWS CLI failed"
        raise LabError(f"{' '.join(command[:8])} …: {detail}")
    text = result.stdout.strip()
    return json.loads(text) if text else None


def atomic_write(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.chmod(temporary, 0o600)
    temporary.replace(path)


def load_state(path: Path) -> dict[str, Any]:
    try:
        state = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as error:
        raise LabError(f"state file does not exist: {path}") from error
    except json.JSONDecodeError as error:
        raise LabError(f"state file is not valid JSON: {path}") from error
    if state.get("version") != STATE_VERSION or state.get("project") != PROJECT_TAG:
        raise LabError(f"refusing a state file outside this lab: {path}")
    return state


def tags(run_id: str, role: str | None = None) -> list[dict[str, str]]:
    values = [
        {"Key": "Project", "Value": PROJECT_TAG},
        {"Key": "RunId", "Value": run_id},
    ]
    if role:
        values.append({"Key": "Role", "Value": role})
    return values


def tag_specifications(*resources: tuple[str, str | None], run_id: str) -> str:
    return json.dumps(
        [{"ResourceType": resource_type, "Tags": tags(run_id, role)} for resource_type, role in resources]
    )


def valid_operator_cidr(value: str) -> str:
    try:
        network = ipaddress.ip_network(value, strict=True)
    except ValueError as error:
        raise argparse.ArgumentTypeError("operator CIDR must be an IPv4 /32") from error
    if network.version != 4 or network.prefixlen != 32:
        raise argparse.ArgumentTypeError("operator CIDR must be an IPv4 /32")
    return str(network)


def valid_run_id(value: str) -> str:
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{2,47}", value):
        raise argparse.ArgumentTypeError("run ID must be 3-48 lowercase letters, digits, or hyphens")
    return value


def read_ssh_public_key(path: Path) -> str:
    value = path.read_text(encoding="utf-8").strip()
    if not value.startswith(("ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-")):
        raise LabError("SSH public key must be an OpenSSH public key")
    return value


def validate_network_plan(args: argparse.Namespace) -> None:
    try:
        vpc = ipaddress.ip_network(args.vpc_cidr, strict=True)
        subnet = ipaddress.ip_network(args.subnet_cidr, strict=True)
    except ValueError as error:
        raise LabError("VPC and subnet CIDRs must be valid IPv4 networks") from error
    if vpc.version != 4 or subnet.version != 4 or not subnet.subnet_of(vpc):
        raise LabError("subnet CIDR must be an IPv4 subnet of the VPC CIDR")
    addresses = [args.relay_private_ip, args.citizen_private_ip, args.provider_private_ip]
    if len(set(addresses)) != len(addresses):
        raise LabError("the three fixed private IP addresses must differ")
    for address in addresses:
        try:
            parsed = ipaddress.ip_address(address)
        except ValueError as error:
            raise LabError(f"invalid private IP address: {address}") from error
        if parsed not in subnet:
            raise LabError(f"private IP is outside the selected subnet: {address}")


def create(args: argparse.Namespace) -> None:
    state_path = args.state.resolve()
    if state_path.exists():
        raise LabError(f"refusing to overwrite existing state: {state_path}")
    validate_network_plan(args)
    read_ssh_public_key(args.ssh_public_key.resolve())
    run_id = args.run_id or valid_run_id(
        "arc-" + dt.datetime.now(dt.timezone.utc).strftime("%Y%m%d%H%M%S") + "-" + secrets.token_hex(3)
    )
    state: dict[str, Any] = {
        "version": STATE_VERSION,
        "project": PROJECT_TAG,
        "run_id": run_id,
        "profile": args.profile,
        "region": args.region,
        "operator_cidr": args.operator_cidr,
        "created_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "resources": {},
    }
    caller = aws(args.profile, args.region, "sts", "get-caller-identity")
    state["account_id"] = caller["Account"]
    atomic_write(state_path, state)

    ami = aws(
        args.profile,
        args.region,
        "ssm",
        "get-parameter",
        "--name",
        args.ami_parameter,
        "--query",
        "Parameter.Value",
        "--output",
        "json",
    )
    state["ami"] = ami
    state["ami_parameter"] = args.ami_parameter
    atomic_write(state_path, state)

    vpc = aws(
        args.profile,
        args.region,
        "ec2",
        "create-vpc",
        "--cidr-block",
        args.vpc_cidr,
        "--tag-specifications",
        tag_specifications(("vpc", None), run_id=run_id),
    )["Vpc"]
    state["resources"]["vpc_id"] = vpc["VpcId"]
    atomic_write(state_path, state)
    aws(args.profile, args.region, "ec2", "wait", "vpc-available", "--vpc-ids", vpc["VpcId"])
    aws(args.profile, args.region, "ec2", "modify-vpc-attribute", "--vpc-id", vpc["VpcId"], "--enable-dns-support", "{\"Value\":true}")
    aws(args.profile, args.region, "ec2", "modify-vpc-attribute", "--vpc-id", vpc["VpcId"], "--enable-dns-hostnames", "{\"Value\":true}")

    availability_zone = args.availability_zone or aws(
        args.profile,
        args.region,
        "ec2",
        "describe-availability-zones",
        "--filters",
        "Name=state,Values=available",
        "--query",
        "AvailabilityZones[0].ZoneName",
        "--output",
        "json",
    )
    state["availability_zone"] = availability_zone
    subnet = aws(
        args.profile,
        args.region,
        "ec2",
        "create-subnet",
        "--vpc-id",
        vpc["VpcId"],
        "--cidr-block",
        args.subnet_cidr,
        "--availability-zone",
        availability_zone,
        "--tag-specifications",
        tag_specifications(("subnet", None), run_id=run_id),
    )["Subnet"]
    state["resources"]["subnet_id"] = subnet["SubnetId"]
    atomic_write(state_path, state)
    aws(args.profile, args.region, "ec2", "modify-subnet-attribute", "--subnet-id", subnet["SubnetId"], "--map-public-ip-on-launch")

    internet_gateway = aws(
        args.profile,
        args.region,
        "ec2",
        "create-internet-gateway",
        "--tag-specifications",
        tag_specifications(("internet-gateway", None), run_id=run_id),
    )["InternetGateway"]
    state["resources"]["internet_gateway_id"] = internet_gateway["InternetGatewayId"]
    atomic_write(state_path, state)
    aws(args.profile, args.region, "ec2", "attach-internet-gateway", "--internet-gateway-id", internet_gateway["InternetGatewayId"], "--vpc-id", vpc["VpcId"])

    route_table = aws(
        args.profile,
        args.region,
        "ec2",
        "create-route-table",
        "--vpc-id",
        vpc["VpcId"],
        "--tag-specifications",
        tag_specifications(("route-table", None), run_id=run_id),
    )["RouteTable"]
    state["resources"]["route_table_id"] = route_table["RouteTableId"]
    atomic_write(state_path, state)
    aws(args.profile, args.region, "ec2", "create-route", "--route-table-id", route_table["RouteTableId"], "--destination-cidr-block", "0.0.0.0/0", "--gateway-id", internet_gateway["InternetGatewayId"])
    association = aws(args.profile, args.region, "ec2", "associate-route-table", "--route-table-id", route_table["RouteTableId"], "--subnet-id", subnet["SubnetId"])
    state["resources"]["route_table_association_id"] = association["AssociationId"]
    atomic_write(state_path, state)

    security_group = aws(
        args.profile,
        args.region,
        "ec2",
        "create-security-group",
        "--group-name",
        f"{PROJECT_TAG}-{run_id}",
        "--description",
        "Temporary ARC traversal verification lab",
        "--vpc-id",
        vpc["VpcId"],
        "--tag-specifications",
        tag_specifications(("security-group", None), run_id=run_id),
    )
    state["resources"]["security_group_id"] = security_group["GroupId"]
    atomic_write(state_path, state)
    aws(args.profile, args.region, "ec2", "authorize-security-group-ingress", "--group-id", security_group["GroupId"], "--ip-permissions", json.dumps([
        {"IpProtocol": "tcp", "FromPort": 22, "ToPort": 22, "IpRanges": [{"CidrIp": args.operator_cidr, "Description": "lab operator SSH"}]},
        {"IpProtocol": "tcp", "FromPort": 1, "ToPort": 65535, "UserIdGroupPairs": [{"GroupId": security_group["GroupId"], "Description": "lab host TCP"}]},
    ]))

    key_name = f"{PROJECT_TAG}-{run_id}"
    imported_key = aws(
        args.profile,
        args.region,
        "ec2",
        "import-key-pair",
        "--key-name",
        key_name,
        "--public-key-material",
        f"fileb://{args.ssh_public_key.resolve()}",
        "--tag-specifications",
        tag_specifications(("key-pair", None), run_id=run_id),
    )
    state["resources"]["key_name"] = key_name
    state["resources"]["key_pair_id"] = imported_key.get("KeyPairId")
    atomic_write(state_path, state)

    # The AWS CLI reads this file and encodes the EC2 blob. Passing an already
    # encoded string would cause cloud-init to receive base64 text as its script.
    user_data = f"file://{Path(__file__).with_name('bootstrap.sh').resolve()}"
    instance_ids: dict[str, str] = {}
    private_addresses = {
        "relay": args.relay_private_ip,
        "citizen": args.citizen_private_ip,
        "provider": args.provider_private_ip,
    }
    for role in ("relay", "citizen", "provider"):
        instance = aws(
            args.profile,
            args.region,
            "ec2",
            "run-instances",
            "--image-id",
            ami,
            "--instance-type",
            "t3.small",
            "--client-token",
            f"{run_id}-{role}",
            "--credit-specification",
            "CpuCredits=standard",
            "--key-name",
            key_name,
            "--subnet-id",
            subnet["SubnetId"],
            "--private-ip-address",
            private_addresses[role],
            "--security-group-ids",
            security_group["GroupId"],
            "--instance-initiated-shutdown-behavior",
            "terminate",
            "--metadata-options",
            "HttpTokens=required,HttpEndpoint=enabled",
            "--block-device-mappings",
            "DeviceName=/dev/sda1,Ebs={VolumeSize=12,VolumeType=gp3,DeleteOnTermination=true}",
            "--user-data",
            user_data,
            "--tag-specifications",
            tag_specifications(("instance", role), ("volume", role), run_id=run_id),
        )["Instances"][0]
        instance_ids[role] = instance["InstanceId"]
        state["resources"]["instance_ids"] = instance_ids
        atomic_write(state_path, state)

    aws(args.profile, args.region, "ec2", "wait", "instance-running", "--instance-ids", *instance_ids.values())
    descriptions = aws(args.profile, args.region, "ec2", "describe-instances", "--instance-ids", *instance_ids.values())["Reservations"]
    by_instance_id = {instance_id: role for role, instance_id in instance_ids.items()}
    public_addresses = {
        by_instance_id[instance["InstanceId"]]: instance.get("PublicIpAddress")
        for reservation in descriptions
        for instance in reservation["Instances"]
    }
    state["resources"]["public_addresses"] = public_addresses
    atomic_write(state_path, state)
    print(json.dumps({"state": str(state_path), "run_id": run_id, "instances": instance_ids, "addresses": public_addresses}, indent=2))


def is_not_found(error: LabError) -> bool:
    return "NotFound" in str(error) or "does not exist" in str(error)


def assert_owned(state: dict[str, Any]) -> None:
    """Reject a state file pointing at a live resource outside this exact run.

    A missing resource is allowed so the same teardown can resume after a
    partial successful cleanup. Every live, directly addressed resource is
    checked before any deletion command runs.
    """
    profile, region, resources = state["profile"], state["region"], state["resources"]
    current_account = aws(profile, region, "sts", "get-caller-identity")["Account"]
    if current_account != state.get("account_id"):
        raise LabError("selected AWS account does not match the run manifest")
    checks: list[tuple[str, list[str]]] = []
    for instance_id in resources.get("instance_ids", {}).values():
        checks.append((instance_id, ["ec2", "describe-instances", "--instance-ids", instance_id]))
    for state_key, command in (
        ("vpc_id", ["ec2", "describe-vpcs", "--vpc-ids"]),
        ("subnet_id", ["ec2", "describe-subnets", "--subnet-ids"]),
        ("security_group_id", ["ec2", "describe-security-groups", "--group-ids"]),
        ("route_table_id", ["ec2", "describe-route-tables", "--route-table-ids"]),
        ("internet_gateway_id", ["ec2", "describe-internet-gateways", "--internet-gateway-ids"]),
    ):
        if resource_id := resources.get(state_key):
            checks.append((resource_id, [*command, resource_id]))
    if key_name := resources.get("key_name"):
        key_pair_id = resources.get("key_pair_id")
        checks.append((key_pair_id or key_name, ["ec2", "describe-key-pairs", "--key-names", key_name]))

    for resource_id, command in checks:
        try:
            aws(profile, region, *command)
        except LabError as error:
            if is_not_found(error):
                continue
            raise
        tag_response = aws(
            profile,
            region,
            "ec2",
            "describe-tags",
            "--filters",
            f"Name=resource-id,Values={resource_id}",
            "Name=key,Values=RunId",
        )
        if not any(tag["Value"] == state["run_id"] for tag in tag_response.get("Tags", [])):
            raise LabError(f"refusing to delete live resource without this run tag: {resource_id}")


def ignore_not_found(action) -> None:  # type: ignore[no-untyped-def]
    try:
        action()
    except LabError as error:
        if not is_not_found(error):
            raise


def teardown(args: argparse.Namespace) -> None:
    state_path = args.state.resolve()
    state = load_state(state_path)
    assert_owned(state)
    profile, region, resources = state["profile"], state["region"], state["resources"]
    instance_ids = list(resources.get("instance_ids", {}).values())
    if instance_ids:
        ignore_not_found(lambda: aws(profile, region, "ec2", "terminate-instances", "--instance-ids", *instance_ids))
        ignore_not_found(lambda: aws(profile, region, "ec2", "wait", "instance-terminated", "--instance-ids", *instance_ids))

    volumes = aws(profile, region, "ec2", "describe-volumes", "--filters", f"Name=tag:RunId,Values={state['run_id']}")["Volumes"]
    for volume in volumes:
        ignore_not_found(lambda volume_id=volume["VolumeId"]: aws(profile, region, "ec2", "delete-volume", "--volume-id", volume_id))

    key_name = resources.get("key_name")
    if key_name:
        ignore_not_found(lambda: aws(profile, region, "ec2", "delete-key-pair", "--key-name", key_name))
    association_id = resources.get("route_table_association_id")
    if association_id:
        ignore_not_found(lambda: aws(profile, region, "ec2", "disassociate-route-table", "--association-id", association_id))
    route_table_id = resources.get("route_table_id")
    if route_table_id:
        ignore_not_found(lambda: aws(profile, region, "ec2", "delete-route-table", "--route-table-id", route_table_id))
    gateway_id, vpc_id = resources.get("internet_gateway_id"), resources.get("vpc_id")
    if gateway_id and vpc_id:
        ignore_not_found(lambda: aws(profile, region, "ec2", "detach-internet-gateway", "--internet-gateway-id", gateway_id, "--vpc-id", vpc_id))
        ignore_not_found(lambda: aws(profile, region, "ec2", "delete-internet-gateway", "--internet-gateway-id", gateway_id))
    group_id = resources.get("security_group_id")
    if group_id:
        ignore_not_found(lambda: aws(profile, region, "ec2", "delete-security-group", "--group-id", group_id))
    subnet_id = resources.get("subnet_id")
    if subnet_id:
        ignore_not_found(lambda: aws(profile, region, "ec2", "delete-subnet", "--subnet-id", subnet_id))
    if vpc_id:
        ignore_not_found(lambda: aws(profile, region, "ec2", "delete-vpc", "--vpc-id", vpc_id))

    remaining_volumes = aws(profile, region, "ec2", "describe-volumes", "--filters", f"Name=tag:RunId,Values={state['run_id']}")["Volumes"]
    if remaining_volumes:
        raise LabError(f"tagged volumes remain: {[volume['VolumeId'] for volume in remaining_volumes]}")
    state["torn_down_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
    atomic_write(state_path, state)
    print(json.dumps({"state": str(state_path), "run_id": state["run_id"], "teardown": "complete"}, indent=2))


def parser() -> argparse.ArgumentParser:
    value = argparse.ArgumentParser(description=__doc__)
    commands = value.add_subparsers(dest="command", required=True)
    for name in ("create", "teardown"):
        command = commands.add_parser(name)
        command.add_argument("--profile", default="default")
        command.add_argument("--region", default=DEFAULT_REGION)
        command.add_argument("--state", type=Path, required=True, help="private local run manifest")
    create_command = commands.choices["create"]
    create_command.add_argument("--operator-cidr", required=True, type=valid_operator_cidr)
    create_command.add_argument("--ssh-public-key", type=Path, required=True)
    create_command.add_argument("--run-id", type=valid_run_id)
    create_command.add_argument("--availability-zone")
    create_command.add_argument("--vpc-cidr", default="10.88.0.0/16")
    create_command.add_argument("--subnet-cidr", default="10.88.0.0/24")
    create_command.add_argument("--relay-private-ip", default="10.88.0.10")
    create_command.add_argument("--citizen-private-ip", default="10.88.0.11")
    create_command.add_argument("--provider-private-ip", default="10.88.0.12")
    create_command.add_argument("--ami-parameter", default=DEFAULT_AMI_PARAMETER)
    return value


def main() -> int:
    args = parser().parse_args()
    try:
        if args.command == "create":
            create(args)
        else:
            teardown(args)
    except (LabError, subprocess.SubprocessError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
