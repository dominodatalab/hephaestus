terraform {
  required_providers {
    aws = {
      version = "~> 4.0"
    }
  }
}

resource "random_string" "name_suffix" {
  length  = 5
  lower   = true
  upper   = false
  numeric = true
  special = false
}

locals {
  name        = "testenv-eks-${random_string.name_suffix.result}"
  vpc_id = aws_vpc.this.id

  kubeconfig = yamlencode({
    apiVersion      = "v1"
    kind            = "Config"
    current-context = "terraform"
    clusters = [{
    name = module.eks.cluster_id
    cluster = {
    certificate-authority-data = module.eks.cluster_certificate_authority_data
    server                     = module.eks.cluster_endpoint
    }
  }]
  contexts = [{
      name = "terraform"
      context = {
      cluster = module.eks.cluster_id
      user    = "terraform"
    }
  }]
  users = [{
        name = "terraform"
        user = {  token = data.aws_eks_cluster_auth.this.token }
    }]
  })
}

# Configure the AWS Provider
provider "aws" {
  region = var.region
}

data "aws_eks_cluster_auth" "this" {
  name = module.eks.cluster_id
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 18.0"

  cluster_name    = "${local.name}-cluster"
  cluster_version = "1.22"

  vpc_id = local.vpc_id
  subnet_ids = [aws_subnet.private_subnet.id, aws_subnet.public_subnet.id]
}


/* VPC + Networking Creation */
resource "aws_vpc" "this" {
  enable_dns_hostnames             = true
  enable_dns_support               = true
  tags = {
    "Name" = "${local.name}-vpc"
  }
}

resource "aws_internet_gateway" "ig" {
  vpc_id = local.vpc_id
  tags = {
    Name        = "${local.name}-igw"
  }
}

resource "aws_eip" "nat_eip" {
  vpc        = true
  depends_on = [aws_internet_gateway.ig]
}

resource "aws_nat_gateway" "nat" {
  allocation_id = aws_eip.nat_eip.id
  subnet_id     = element(aws_subnet.public_subnet.*.id, 0)
  depends_on    = [aws_internet_gateway.ig]
  tags = {
    Name        = "${local.name}-nat-gateway"
  }
}

/* Subnets */
resource "aws_subnet" "public_subnet" {
  vpc_id            = local.vpc_id
  tags = {
    Name        = "${local.name}-public-subnet"
  }
}

resource "aws_subnet" "private_subnet" {
  vpc_id                  = local.vpc_id
  tags = {
    Name        = "${local.name}-private-subnet"
  }
}
/* Route Table */
resource "aws_route_table" "private" {
  vpc_id = local.vpc_id
  tags = {
    Name        = "${local.name}-private-route-table"
  }
}

resource "aws_route_table" "public" {
  vpc_id = local.vpc_id
  tags = {
    Name        = "${local.name}-public-route-table"
  }
}

/* Routes */
resource "aws_route" "public_internet_gateway" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.ig.id
}

resource "aws_route" "private_nat_gateway" {
  route_table_id         = aws_route_table.private.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.nat.id
}

/* Route table association */
resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public_subnet.id
  route_table_id = aws_route_table.public.id
}
resource "aws_route_table_association" "private" {
  subnet_id      = aws_subnet.private_subnet.id
  route_table_id = aws_route_table.private.id
}

/* VPC's Default Security Group */
resource "aws_security_group" "default" {
  name        = "${local.name}-default-sg"
  description = "Default security group to allow inbound/outbound traffic from the VPC"
  vpc_id      = local.vpc_id
  depends_on  = [aws_vpc.this]

  ingress {
    from_port = "0"
    to_port   = "0"
    protocol  = "-1"
    self      = true
  }

  egress {
    from_port = "0"
    to_port   = "0"
    protocol  = "-1"
    self      = "true"
  }
}