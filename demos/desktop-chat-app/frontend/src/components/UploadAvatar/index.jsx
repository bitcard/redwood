import React, { useRef, useState } from 'react'
import styled from 'styled-components'
import { PhotoCamera as PhotoCameraIcon, AddCircle as AddCircleIcon } from '@material-ui/icons'
import { makeStyles } from '@material-ui/core/styles'

import theme from '../../theme'

const SUploadAvatarContainer = styled.div`
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
`

const useCircleIconStyles = makeStyles({
    root: {
        color: theme.color.indigo[500],
        position: 'absolute',
        top: props => props.iconPlacement === 'bottom right' ? '52px' : '-19px',
        left: props => props.iconPlacement === 'bottom right' ? '59px' : 'unset',
    }
})

const SUploadAvatarCircle = styled.div`
  height: 72px;
  width: 72px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  border: 2px dashed ${theme.color.white};
  border-radius: 100%;
  position: relative;
  transition: all ease-in-out .15s;
  cursor: pointer;
  background-image: ${props => props.avatarPreview ? `url('${props.avatarPreview}')` : 'none'};
  &:hover {
    background: rgba(255, 255, 255, .12);
    transform: scale(1.1);
  }
  span {
    color: ${theme.color.white};
    font-size: 12px;
    text-align: center;
  }
`

const SAvatarOverflow = styled.div`
  height: 72px;
  width: 72px;
  overflow: hidden;
  border-radius: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
`

const SAvatarImg = styled.img`
  height: 72px;
`

function UploadAvatar({
  iconImg,
  setIconImg,
  setIconFile,
}) {
  const fileUploadRef = useRef(null)
  const circleIconClasses = useCircleIconStyles({ iconPlacement: 'bottom right' })

  const handleFileUpload = () => {
    fileUploadRef.current.click()
  }

  const onFileChange = () => {
    const [ file ] = fileUploadRef.current.files

    const reader = new FileReader()
    reader.addEventListener('load', () => {
      setIconImg(reader.result)
    }, false)

    if (file) {
      reader.readAsDataURL(file)
      setIconFile(file)
    }
  }

  let content = (
    <SUploadAvatarCircle onClick={handleFileUpload}>
      <AddCircleIcon classes={circleIconClasses} />
      <PhotoCameraIcon style={{ color: theme.color.white }} />
      <span>UPLOAD</span>
    </SUploadAvatarCircle>
  )

  if (iconImg) {
    content = (
      <SUploadAvatarCircle onClick={handleFileUpload}>
        <AddCircleIcon classes={circleIconClasses} />
        <SAvatarOverflow>
          <SAvatarImg src={iconImg} />
        </SAvatarOverflow>
      </SUploadAvatarCircle>
    )
  }

  return (
    <SUploadAvatarContainer>
      { content }
      <input
        style={{ display: 'none' }}
        ref={fileUploadRef}
        type="file"
        onChange={onFileChange}
      />
    </SUploadAvatarContainer>
  )
}

export default UploadAvatar