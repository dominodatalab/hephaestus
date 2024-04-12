resource "random_string" "name_suffix" {
  length  = 5
  lower   = true
  upper   = false
  numeric = true
  special = false
}

locals {
  name            = "testenv-eks-${random_string.name_suffix.result}"
  kubeconfig_file = "${path.module}/kubeconfig"
  cluster_name    = "${local.name}-cluster"
}

provider "aws" {
  region = var.region
}

data "aws_availability_zones" "available" {
  state = "available"
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.7"

  enable_nat_gateway = true
  cidr               = "10.0.0.0/16"

  enable_dns_hostnames = true

  name = "${local.name}-vpc"
  azs = [
    data.aws_availability_zones.available.names[0],
    data.aws_availability_zones.available.names[1]
  ]
  public_subnets  = ["10.0.1.0/24", "10.0.2.0/24"]
  private_subnets = ["10.0.101.0/24", "10.0.102.0/24"]
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.8"

  cluster_name    = local.cluster_name
  cluster_version = var.kubernetes_version

  cluster_endpoint_public_access           = true
  enable_cluster_creator_admin_permissions = true

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  eks_managed_node_group_defaults = {
    iam_role_additional_policies = {
      ecr            = aws_iam_policy.ecr_policy.arn,
      nodes          = aws_iam_policy.node_policy.arn,
      ebs-csi-driver = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
    }
  }

  eks_managed_node_groups = {
    default = {
      min_size     = 2
      max_size     = 10
      desired_size = 2

      instance_types = ["t3.xlarge"]
      capacity_type  = "SPOT"
    }
  }

  cluster_addons = {
    vpc-cni = {
      configuration_values = jsonencode({
        env = {
          ANNOTATE_POD_IP = "true"
        }
      })
    }
    aws-ebs-csi-driver = {
      resolve_conflicts = "OVERWRITE"
    }
  }

  # NOTE: This may not be required but requires testing
  cluster_security_group_additional_rules = {
    ingress_self_all = {
      description = "Cluster to node all ports/protocols"
      protocol    = "-1"
      from_port   = 0
      to_port     = 0
      type        = "ingress"
      self        = true
    }
    egress_all = {
      description      = "Cluster all egress"
      protocol         = "-1"
      from_port        = 0
      to_port          = 0
      type             = "egress"
      cidr_blocks      = ["0.0.0.0/0"]
      ipv6_cidr_blocks = ["::/0"]
    }
  }

  node_security_group_additional_rules = {
    ingress_cluster_istio_webhooks = {
      description                   = "Cluster API to node Istio webhooks"
      protocol                      = "TCP"
      from_port                     = 15017
      to_port                       = 15017
      type                          = "ingress"
      source_cluster_security_group = true
    }
    ingress_cluster_cert_manager_webhooks = {
      description                   = "Cluster API to node cert-manager webhooks"
      protocol                      = "TCP"
      from_port                     = 10260
      to_port                       = 10260
      type                          = "ingress"
      source_cluster_security_group = true
    }
    # NOTE: this may not be required but requires testing
    ingress_self_all = {
      description = "Node to node all ports/protocols"
      protocol    = "-1"
      from_port   = 0
      to_port     = 0
      type        = "ingress"
      self        = true
    }
    egress_all = {
      description      = "Node all egress"
      protocol         = "-1"
      from_port        = 0
      to_port          = 0
      type             = "egress"
      cidr_blocks      = ["0.0.0.0/0"]
      ipv6_cidr_blocks = ["::/0"]
    }
  }
}

module "ecr" {
  source  = "terraform-aws-modules/ecr/aws"
  version = "~> 2.2"

  repository_name          = "${local.name}-ecr-repository"
  repository_force_delete  = true
  attach_repository_policy = false

  repository_lifecycle_policy = jsonencode({
    rules = [
      {
        rulePriority = 1,
        description  = "Keep last 30 images",
        selection = {
          tagStatus     = "tagged",
          tagPrefixList = ["v"],
          countType     = "imageCountMoreThan",
          countNumber   = 30
        },
        action = {
          type = "expire"
        }
      }
    ]
  })
}

data "aws_iam_policy_document" "ecr_access_policy_document" {
  statement {
    sid       = "GetAuthorizationToken"
    actions   = ["ecr:GetAuthorizationToken"]
    effect    = "Allow"
    resources = ["*"]
  }
  statement {
    sid = "ManageRepositoryContents"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:GetRepositoryPolicy",
      "ecr:DescribeRepositories",
      "ecr:ListImages",
      "ecr:DescribeImages",
      "ecr:BatchGetImage",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
      "ecr:PutImage"
    ]
    effect    = "Allow"
    resources = [module.ecr.repository_arn]
  }
}

data "aws_iam_policy_document" "node_access_policy_document" {
  statement {
    sid = "NodeAccessPolicy"
    actions = [
      "ec2:Describe*"
    ]
    effect    = "Allow"
    resources = ["*"]
  }
}

resource "aws_iam_policy" "node_policy" {
  name   = "${local.name}-node-access-policy"
  policy = data.aws_iam_policy_document.node_access_policy_document.json
}

resource "aws_iam_policy" "ecr_policy" {
  name   = "${local.name}-ecr-access-policy"
  policy = data.aws_iam_policy_document.ecr_access_policy_document.json
}

resource "terraform_data" "kubeconfig" {
  provisioner "local-exec" {
    when    = create
    command = "aws eks update-kubeconfig --kubeconfig ${local.kubeconfig_file} --region ${var.region} --name ${local.cluster_name}"
  }

  triggers_replace = [
    module.eks.cluster_certificate_authority_data,
    local.cluster_name,
    local.kubeconfig_file,
    var.region
  ]
}

data "local_file" "kubeconfig" {
  filename   = local.kubeconfig_file
  depends_on = [terraform_data.kubeconfig]
}
