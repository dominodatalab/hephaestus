resource "random_string" "name_suffix" {
  length  = 5
  lower   = true
  upper   = false
  numeric = true
  special = false
}

locals {
  name         = "testenv-eks-${random_string.name_suffix.result}"
  cluster_name = "${local.name}-cluster"
}

provider "aws" {
  region = var.region
}

provider "kubernetes" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)

  exec {
    api_version = "client.authentication.k8s.io/v1"
    command     = "aws"
    # This requires the awscli to be installed locally where Terraform is executed
    args = ["eks", "get-token", "--cluster-name", module.eks.cluster_id, "--region", var.region]
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_eks_cluster_auth" "this" {
  name = module.eks.cluster_id
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 3.18"

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
  version = "~> 18.30"

  cluster_name    = local.cluster_name
  cluster_version = var.kubernetes_version

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  manage_aws_auth_configmap = true

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
    ingress_cluster_hephaestus_webhooks = {
      description                   = "Cluster API to node Hephaestus webhooks"
      protocol                      = "TCP"
      from_port                     = 9443
      to_port                       = 9443
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
  version = "~> 1.4"

  repository_name         = "${local.name}-ecr-repository"
  repository_force_delete = true

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

resource "aws_iam_role_policy_attachment" "ebs_csi_driver" {
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
  role       = module.eks.eks_managed_node_groups["default"].iam_role_name
}

resource "aws_iam_role_policy_attachment" "ecr_access" {
  policy_arn = aws_iam_policy.ecr_policy.arn
  role       = module.eks.eks_managed_node_groups["default"].iam_role_name
}

resource "aws_iam_role_policy_attachment" "node_access" {
  policy_arn = aws_iam_policy.node_policy.arn
  role       = module.eks.cluster_iam_role_name
}

resource "null_resource" "kubeconfig" {
  provisioner "local-exec" {
    when    = create
    command = "aws eks update-kubeconfig --kubeconfig ${self.triggers.kubeconfig_file} --region ${self.triggers.region} --name ${self.triggers.cluster_name}"
  }

  triggers = {
    domino_eks_cluster_ca = module.eks.cluster_certificate_authority_data
    cluster_name          = local.cluster_name
    kubeconfig_file       = "${path.module}/kubeconfig"
    region                = var.region
  }
}

data "local_file" "kubeconfig" {
  filename = null_resource.kubeconfig.triggers.kubeconfig_file
}
