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

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_eks_cluster_auth" "this" {
  name = module.eks.cluster_id
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 3.18"

  cidr = "10.0.0.0/16"

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

  eks_managed_node_groups = {
    default = {
      min_size     = 2
      max_size     = 10
      desired_size = 2

      instance_types = ["t3.xlarge"]
      capacity_type  = "SPOT"
    }
  }
}

module "ecr" {
  source          = "terraform-aws-modules/ecr/aws"
  version         = "~> 1.4"
  repository_name = "${local.name}-ecr-repository"

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

data "aws_iam_policy_document" "ecr_access" {
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

resource "aws_iam_policy" "ecr_policy" {
  name   = "${local.name}-ecr-access-policy"
  policy = data.aws_iam_policy_document.ecr_access.json
}

resource "aws_iam_role_policy_attachment" "ecr_access" {
  policy_arn = aws_iam_policy.ecr_policy.arn
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
    kubeconfig_file       = var.kubeconfig_path
    region                = var.region
  }
}
